package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
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
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-xray-sdk-go/xray"
	"github.com/cenkalti/backoff"
	"github.com/oklog/ulid"
	"go.uber.org/zap"
)

// DataReader is a
type DataReader struct {
	input state.Schema
	sess  client.ConfigProvider
	ctx   context.Context
	err   error
}

func (dr *DataReader) getSession() (sess client.ConfigProvider) {
	logger := log.Logger(dr.ctx)

	if dr.sess != nil {
		return dr.sess
	}

	config := &aws.Config{
		Region:     aws.String(dr.input.Region),
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
	dr.sess = sess

	return

}

func (dr *DataReader) storeItems(items []map[string]*dynamodb.AttributeValue) (storageID string, err error) {

	logger := log.Logger(dr.ctx)

	outBuffer := bytes.NewBufferString("")

	var records []map[string]interface{}

	errUnmarshal := dynamodbattribute.UnmarshalListOfMaps(items, &records)

	if errUnmarshal != nil {
		logger.Panic("failed to unmarshal dynamodb scan items", zap.Error(err))
		return "", errUnmarshal
	}

	for _, record := range records {

		b, errJSON := json.Marshal(record)
		if errJSON != nil {
			logger.Panic("unable marshal record into JSON", zap.Error(errJSON))
		}

		outBuffer.Write(b)
		outBuffer.WriteString("\n")
	}

	logger.Info("storing items", zap.Int("records", len(records)))

	// build a ULID
	t := time.Now().UTC()
	entropy := rand.New(rand.NewSource(t.UnixNano()))
	id := ulid.MustNew(ulid.Timestamp(t), entropy)

	storageID = id.String()

	fileName := fmt.Sprintf("%v/%v.json", dr.input.OrigTableName, storageID)

	s3Svc := s3.New(dr.getSession())
	xray.AWS(s3Svc.Client)

	// Create s3 Client
	uploader := s3manager.NewUploaderWithClient(s3Svc)

	_, err = uploader.UploadWithContext(dr.ctx, &s3manager.UploadInput{
		Bucket: aws.String(dr.input.Bucket),
		Key:    aws.String(fileName),
		Body:   outBuffer,
	})

	if err != nil {
		logger.Panic(fmt.Sprintf("unable to upload %s to %s", fileName, dr.input.Bucket), zap.Error(err))
	}

	logger.Info(fmt.Sprintf("successfully uploaded %s to %s", fileName, dr.input.Bucket))

	return
}

func (dr *DataReader) dynamodbScan() (output state.ExportResult, err error) {

	logger := log.Logger(dr.ctx)

	// https://docs.aws.amazon.com/lambda/latest/dg/golang-context.html
	deadline, _ := dr.ctx.Deadline()
	deadline = deadline.Add(-3000 * time.Millisecond)
	timeoutChannel := time.After(time.Until(deadline))

	// Create DynamoDB client
	svc := dynamodb.New(dr.getSession())

	xray.AWS(svc.Client)

	expbo := backoff.NewExponentialBackOff()
	expbo.MaxInterval = 1500 * time.Millisecond
	boff := backoff.WithContext(expbo, dr.ctx)

	logger.Info(fmt.Sprintf("scanning table %s", dr.input.OrigTableName))

	// have we got previous results ?
	if dr.input.Export.LastKey != nil {
		output = dr.input.Export
	}

	for {

		select {

		case <-timeoutChannel:

			totalReads := output.Processed - dr.input.Export.Processed

			logger.Warn("data export lambda duration expired", zap.Int64("reads", totalReads))

			return

		default:

			// scan params
			params := &dynamodb.ScanInput{
				TableName:     aws.String(dr.input.OrigTableName),
				Segment:       aws.Int64(dr.input.ExportConfig.Segment),
				TotalSegments: aws.Int64(dr.input.ExportConfig.TotalSegments),
				Limit:         aws.Int64(dr.input.ExportConfig.Limit),
			}

			// last evaluated key
			if output.LastKey != nil {
				params.ExclusiveStartKey = output.LastKey
			}

			// scan, sleep if rate limited
			resp, scanErr := svc.ScanWithContext(dr.ctx, params)

			if scanErr != nil {
				if awsErr, ok := scanErr.(awserr.Error); ok {
					// process SDK error
					switch awsErr.Code() {
					case dynamodb.ErrCodeProvisionedThroughputExceededException, dynamodb.ErrCodeRequestLimitExceeded:

						logger.Warn("thoughput error backing off", zap.Int64("itemcount", output.Processed), zap.Error(scanErr))

						// need to sleep when re-requesting, per spec
						if err := aws.SleepWithContext(dr.ctx, boff.NextBackOff()); err != nil {
							logger.Panic("timed out", zap.Error(err))
						}
						continue
					default:
						logger.Panic("unknown dynamodb error", zap.Error(scanErr))
					}

				} else {
					logger.Panic("unknown error", zap.Error(scanErr))
				}
			}

			// reset backoff
			boff.Reset()

			// call the handler function with items
			storageID, storeError := dr.storeItems(resp.Items)

			if storeError != nil {
				logger.Fatal("item store failed", zap.Error(storeError))
			}

			logger.Info("items stored", zap.Int64("items", int64(len(resp.Items))))

			// add storage id
			output.Records = append(output.Records, storageID)

			// add to tally
			output.Processed += int64(len(resp.Items))

			// set last evaluated key
			output.LastKey = resp.LastEvaluatedKey

			// exit if last evaluated key empty
			if output.LastKey == nil {
				output.Complete = true
				return
			}

		}

	}
}

// Run executes a batch batch.
func (dr *DataReader) Run() (output state.ExportResult, err error) {
	return dr.dynamodbScan()
}

// Handler is foo
func Handler(ctx context.Context, input state.Schema) (output state.ExportResult, err error) {

	lc, _ := lambdacontext.FromContext(ctx)

	rqCtx := log.WithRqID(ctx, lc.AwsRequestID)

	logger := log.Logger(rqCtx).With(zap.String("sourceRegion", input.Region),
		zap.String("sourceBucket", input.Bucket),
		zap.String("tableName", input.OrigTableName),
	)

	xray.SetLogger(&log.XrayLogger{})

	xray.Configure(xray.Config{
		LogLevel:       "info", // default
		ServiceVersion: "1.2.3",
	})

	// Default to 10000 items in scan
	if input.ExportConfig.Limit < 1 {
		input.ExportConfig.Limit = 10000
	}
	// default to a single segment scan
	if input.ExportConfig.TotalSegments < 1 {
		input.ExportConfig.TotalSegments = 1
	}

	logger.Info("dyanmodb data export handler")

	start := time.Now()

	reader := DataReader{
		input: input,
		ctx:   rqCtx,
	}

	output, err = reader.Run()

	if err != nil {
		logger.Panic("full scan failed", zap.Error(err))
	}

	output.DurationMS = time.Now().Sub(start).Milliseconds()

	logger.Info("complete", zap.Int64("duration", output.DurationMS), zap.Int64("items", output.Processed))

	return

}

func main() {
	lambda.Start(Handler)
}
