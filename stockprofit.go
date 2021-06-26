package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/ses"
)

type Ticker struct {
	Symble string  `json:"symble"`
	Bid    float64 `json:"bid"`
	Value  float64 `json:"value"`
	Hold   int     `json:"hold"`
}

type Result struct {
	CreatedAt string   `json:"created_at"`
	Body      []Ticker `json:"body"`
}

var r = regexp.MustCompile(`watchlist(\d+.\d+)`)

func main() {
	lambda.Start(Handler)
}

// Handler is lambda function start point.
func Handler(request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// response
	res := events.APIGatewayProxyResponse{}

	// check api key
	if request.Headers["stock-api-key"] != os.Getenv("STOCK_API_KEY") {
		res.StatusCode = http.StatusBadRequest
		res.Body = "status bad request."
		return res, fmt.Errorf("status bad request. %d", http.StatusBadRequest)
	}

	activeThreads := 0
	doneTicker := make(chan Ticker)

	var tickers []Ticker
	for _, symbol := range GetTickerSymbles() {
		go GetStockPrice(symbol, doneTicker)
		activeThreads++
	}

	for activeThreads > 0 {
		tickers = append(tickers, <-doneTicker)
		activeThreads--
	}

	t := time.Now().Local()
	result := Result{
		CreatedAt: t.Format("2006-01-02"),
		Body:      tickers,
	}

	// make json
	b, err := json.Marshal(result)
	if err != nil {
		res.StatusCode = http.StatusInternalServerError
		res.Body = err.Error()
		return res, err
	}

	// file upload to s3
	if err := UploadFile(b, t); err != nil {
		res.StatusCode = http.StatusInternalServerError
		res.Body = err.Error()
		return res, err
	}

	// send mail
	if err := SenderMail(result); err != nil {
		fmt.Println(err)
	}

	res.StatusCode = http.StatusOK
	res.Body = string(b)
	return res, nil
}

// GetTickerSymbles is my stock symbole.
func GetTickerSymbles() []Ticker {
	return []Ticker{
		{"AMD", 84.81, 0.0, 20},
		{"AMZN", 3143.47, 0.0, 3},
		{"COST", 321.17, 0.0, 5},
		{"CRM", 223.67, 0.0, 5},
		{"GOOGL", 2058.39, 0.0, 2},
		{"LMT", 333.27, 0.0, 10},
		{"NVDA", 556.82, 0.0, 5},
		{"OPEN", 22.51, 0.0, 10},
		{"PYPL", 265.08, 0.0, 5},
		{"SPCE", 32.71, 0.0, 10},
		{"V", 216.96, 0.0, 5},
		{"ZG", 169.71, 0.0, 10},
	}
}

// UploadFile is an uploader, make json file to S3 upload.
func UploadFile(b []byte, t time.Time) error {
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(endpoints.ApNortheast1RegionID),
	})
	if err != nil {
		return err
	}

	filePath := fmt.Sprintf(os.Getenv("S3_FILE_PATH"), t.Year(), t.Month())
	uploader := s3manager.NewUploader(sess)
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(os.Getenv("BUCKET")),
		Key:    aws.String(filePath),
		Body:   bytes.NewReader(b),
	})
	if err != nil {
		return err
	}
	return nil
}

// GetStockPrice is get stock price from yahoo finance web page.
func GetStockPrice(symbol Ticker, doneTicker chan Ticker) {
	url := fmt.Sprintf("https://finance.yahoo.com/quote/%s", symbol.Symble)
	doc, err := goquery.NewDocument(url)
	if err != nil {
		doneTicker <- Ticker{}
		return
	}
	text := doc.Find("div#quote-header-info").Text()
	text = strings.ReplaceAll(text, ",", "")

	result := r.FindAllStringSubmatch(text, -1)

	f, err := strconv.ParseFloat(result[0][1], 64)
	if err != nil {
		doneTicker <- Ticker{}
		return
	}

	ticker := Ticker{
		Symble: symbol.Symble,
		Bid:    symbol.Bid,
		Value:  f,
		Hold:   symbol.Hold,
	}
	doneTicker <- ticker
}

// send report mail
func SenderMail(result Result) error {
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(endpoints.ApNortheast1RegionID),
	})
	if err != nil {
		return err
	}

	var sum float64
	var content string
	for _, r := range result.Body {
		earn := (r.Value - r.Bid) * float64(r.Hold)
		c := fmt.Sprintf("%s %10.2f %10.2f %6d %10.2f\n",
			r.Symble, r.Bid, r.Value, r.Hold, earn)
		content = content + c
		sum += earn
	}
	content = content + fmt.Sprintln(strings.Repeat("-", 30))
	content = content + fmt.Sprintf("%sProfit Loss: %10.2f\n", strings.Repeat(" ", 27), sum)

	svc := ses.New(sess)
	input := &ses.SendEmailInput{
		Destination: &ses.Destination{
			ToAddresses: []*string{
				aws.String(os.Getenv("MAIL_TO_ADDRESS")),
			},
		},
		Message: &ses.Message{
			Body: &ses.Body{
				Text: &ses.Content{
					Charset: aws.String("UTF-8"),
					Data:    aws.String(content),
				},
			},
			Subject: &ses.Content{
				Charset: aws.String("UTF-8"),
				Data:    aws.String(os.Getenv("MAIL_SUBJECT")),
			},
		},
		Source: aws.String(os.Getenv("MAIL_SENDER_ADDRESS")),
	}

	_, err = svc.SendEmail(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case ses.ErrCodeMessageRejected:
				return fmt.Errorf("%s, %s", ses.ErrCodeMessageRejected, aerr.Error())
			case ses.ErrCodeMailFromDomainNotVerifiedException:
				return fmt.Errorf("%s, %s", ses.ErrCodeMailFromDomainNotVerifiedException, aerr.Error())
			case ses.ErrCodeConfigurationSetDoesNotExistException:
				return fmt.Errorf("%s, %s", ses.ErrCodeConfigurationSetDoesNotExistException, aerr.Error())
			default:
				return fmt.Errorf("%s", aerr.Error())
			}
		} else {
			return fmt.Errorf("%s", err.Error())
		}
	}
	return nil
}
