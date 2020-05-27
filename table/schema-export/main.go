package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/NixM0nk3y/dynamodb-clone/log"
	"github.com/NixM0nk3y/dynamodb-clone/state"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-xray-sdk-go/xray"
	"go.uber.org/zap"
)

// SchemaReader is a
type SchemaReader struct {
	input state.Schema
	sess  client.ConfigProvider
	ctx   context.Context
	err   error
}

func (sr *SchemaReader) getSession() (sess client.ConfigProvider) {
	logger := log.Logger(sr.ctx)

	if sr.sess != nil {
		return sr.sess
	}

	config := &aws.Config{
		Region:     aws.String(sr.input.Region),
		MaxRetries: aws.Int(5),
		Logger:     &log.AWSLogger{},
		LogLevel:   log.AWSLevel(),
	}

	// override endpoint supplied
	if awsEndpoint := os.Getenv("AWS_ENDPOINT"); awsEndpoint != "" {
		logger.Info(fmt.Sprintf("setting endpoint to %s", awsEndpoint))
		config.Endpoint = aws.String(awsEndpoint)
	}

	// override endpoint supplied
	if awsS3pathstyle := os.Getenv("AWS_S3_FORCEPATHSTYLE"); awsS3pathstyle != "" {
		logger.Info("setting S3 to pathstyle")
		config.S3ForcePathStyle = aws.Bool(true)
	}

	sess, err := session.NewSession(config)

	if err != nil {
		logger.Panic("unable generate new session", zap.Error(err))
	}

	// stash the session
	sr.sess = sess

	return
}

func (sr *SchemaReader) storeSchema(schema *dynamodb.DescribeTableOutput) (result bool, err error) {

	logger := log.Logger(sr.ctx)

	outBuffer := bytes.NewBufferString("")

	b, errJSON := json.Marshal(schema)

	if errJSON != nil {
		logger.Panic("unable to marshal record into JSON", zap.Error(errJSON))
	}

	outBuffer.Write(b)

	logger.Info(fmt.Sprintf("storing schema for %s", sr.input.OrigTableName))

	fileName := fmt.Sprintf("%v/schema.json", sr.input.OrigTableName)

	s3Svc := s3.New(sr.getSession())
	xray.AWS(s3Svc.Client)

	// Create s3 Client
	uploader := s3manager.NewUploaderWithClient(s3Svc)

	_, err = uploader.UploadWithContext(sr.ctx, &s3manager.UploadInput{
		Bucket: aws.String(sr.input.Bucket),
		Key:    aws.String(fileName),
		Body:   outBuffer,
	})

	if err != nil {
		logger.Panic(fmt.Sprintf("unable to upload %s to %s", fileName, sr.input.Bucket), zap.Error(err))
	} else {
		logger.Info(fmt.Sprintf("successfully uploaded %s to %s", fileName, sr.input.Bucket))
		result = true
	}

	return
}

//
func (sr *SchemaReader) dynamodbSchemaExport() (result bool, err error) {

	logger := log.Logger(sr.ctx)

	// Create DynamoDB client
	svc := dynamodb.New(sr.getSession())

	xray.AWS(svc.Client)

	logger.Info("pulling table schema")

	tableInput := &dynamodb.DescribeTableInput{
		TableName: aws.String(sr.input.OrigTableName),
	}

	table, describeError := svc.DescribeTableWithContext(sr.ctx, tableInput)
	if describeError != nil {
		if aerr, ok := describeError.(awserr.Error); ok {
			switch aerr.Code() {
			case dynamodb.ErrCodeResourceNotFoundException:
				logger.Panic("table not found", zap.Error(describeError))
			default:
				logger.Panic("dynamodb returned error", zap.Error(describeError))
			}
		} else {
			logger.Panic("unknown error", zap.Error(describeError))
		}
		return false, describeError
	}

	return sr.storeSchema(table)
}

// Run executes a export of the schema.
func (sr *SchemaReader) Run() (complete bool, err error) {
	return sr.dynamodbSchemaExport()
}

// Handler is foo
func Handler(ctx context.Context, input state.Schema) (output state.SchemaResult, err error) {

	lc, _ := lambdacontext.FromContext(ctx)

	rqCtx := log.WithRqID(ctx, lc.AwsRequestID)

	logger := log.Logger(rqCtx).With(zap.String("region", input.Region),
		zap.String("bucket", input.Bucket),
		zap.String("table", input.OrigTableName),
	)

	xray.SetLogger(&log.XrayLogger{})

	xray.Configure(xray.Config{
		LogLevel:       "info", // default
		ServiceVersion: "1.2.3",
	})

	logger.Info("dyanmodb table schema export")

	var exportError error

	reader := SchemaReader{
		input: input,
		ctx:   rqCtx,
	}

	start := time.Now()

	output.Complete, exportError = reader.Run()

	if exportError != nil {
		logger.Panic("schema export failed", zap.Error(err))
	}

	output.DurationMS = time.Now().Sub(start).Milliseconds()

	logger.Info("complete", zap.Int64("duration", output.DurationMS))

	return

}

func main() {
	lambda.Start(Handler)
}
