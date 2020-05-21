package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/NixM0nk3y/dynamodb-clone/log"
	"github.com/NixM0nk3y/dynamodb-clone/state"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-xray-sdk-go/xray"
	"github.com/cenkalti/backoff"
	"go.uber.org/zap"
)

// DataWriter is a
type DataWriter struct {
	input state.Schema
	ctx   context.Context
	err   error
}

func (dw *DataWriter) retrieveData(key string) (records []map[string]*dynamodb.AttributeValue, err error) {

	logger := log.Logger(dw.ctx)

	logger.Info(fmt.Sprintf("retrieving data key %s for %s", dw.input.OrigTableName, key))

	sess, err := session.NewSession(&aws.Config{
		Region:     aws.String(dw.input.Region),
		MaxRetries: aws.Int(5),
		Logger:     &log.AWSLogger{},
		LogLevel:   log.AWSLevel(),
	})

	if err != nil {
		return nil, err
	}

	fileName := fmt.Sprintf("%s/%s.json", dw.input.OrigTableName, key)

	s3Svc := s3.New(sess)
	xray.AWS(s3Svc.Client)

	// Create s3 Client
	downLoader := s3manager.NewDownloaderWithClient(s3Svc)

	w := &aws.WriteAtBuffer{}

	_, downloadErr := downLoader.DownloadWithContext(dw.ctx, w, &s3.GetObjectInput{
		Bucket: aws.String(dw.input.Bucket),
		Key:    aws.String(fileName),
	})

	if downloadErr != nil {
		logger.Panic(fmt.Sprintf("unable to download records file %s from s3://%s", fileName, dw.input.Bucket), zap.Error(downloadErr))
	}

	logger.Info(fmt.Sprintf("successfully retrieved records file %s from s3://%s", fileName, dw.input.Bucket))

	s := bufio.NewScanner(bytes.NewReader(w.Bytes()))

	for s.Scan() {
		var item map[string]interface{}
		if errJSON := json.Unmarshal(s.Bytes(), &item); err != nil {
			logger.Panic("unable unmarshal record from JSON", zap.Error(errJSON))
		}
		av, attributeErr := dynamodbattribute.MarshalMap(item)
		if attributeErr != nil {
			logger.Panic("failed to dynamodb marshal record", zap.Error(attributeErr))
		}

		records = append(records, av)
	}

	if scanErr := s.Err(); scanErr != nil {
		logger.Panic("unable unmarshal record from JSON", zap.Error(scanErr))
	}

	return
}

// dynamodbScan
func (dw *DataWriter) dynamodbImport() (output state.ImportResult, err error) {

	logger := log.Logger(dw.ctx)

	// https://docs.aws.amazon.com/lambda/latest/dg/golang-context.html
	deadline, _ := dw.ctx.Deadline()
	deadline = deadline.Add(-2500 * time.Millisecond) // dynamoDB retries take a while to return
	timeoutChannel := time.After(time.Until(deadline))

	sess, err := session.NewSession(&aws.Config{
		Region:     aws.String(dw.input.Region),
		MaxRetries: aws.Int(5),
		Logger:     &log.AWSLogger{},
		LogLevel:   log.AWSLevel(),
	})

	if err != nil {
		return
	}

	// Create DynamoDB client
	svc := dynamodb.New(sess)

	xray.AWS(svc.Client)

	expbo := backoff.NewExponentialBackOff()
	expbo.MaxInterval = 1500 * time.Millisecond
	boff := backoff.WithContext(expbo, dw.ctx)

	logger.Info(fmt.Sprintf("importing data into table %s", dw.input.NewTableName))

	output.Records = dw.input.Import.Records

	data, retrieveErr := dw.retrieveData(output.Records)

	if retrieveErr != nil {
		logger.Panic("error retrieving data", zap.Error(retrieveErr))
	}

	logger.Info(fmt.Sprintf("successfully retrieved %d records from %s", len(data), output.Records))

	//
	output.Processed = dw.input.Import.Processed

	logger.Info(fmt.Sprintf("starting processing from record %d", dw.input.Import.Processed))

	// write status tracking
	ticker := time.NewTicker(5000 * time.Millisecond)
	lastTick := time.Now()
	lastProcesed := output.Processed

	// batch loop through the data
nextbatch:
	for start := output.Processed; start < int64(len(data)); start += dw.input.ImportConfig.BatchSize {

		// end of current batch
		end := start + int64(dw.input.ImportConfig.BatchSize)

		// end of slice
		if end > int64(len(data)) {
			end = int64(len(data))
		}

		records := data[start:end]

		// build our write request
		writeRequests := make([]*dynamodb.WriteRequest, len(records))
		for i := 0; i < len(records); i++ {
			writeRequests[i] = &dynamodb.WriteRequest{
				PutRequest: &dynamodb.PutRequest{
					Item: records[i],
				},
			}
		}

		// write retry loop
		for {
			select {

			case t := <-ticker.C:

				tickPeriod := t.Sub(lastTick).Milliseconds()

				// records processed this tick
				tickProcessed := output.Processed - lastProcesed

				// round to two decimal places
				rate := math.Round((float64(tickProcessed)/(float64(tickPeriod)/1000))*100) / 100

				logger.Info("processing writes", zap.Int64("items", output.Processed),
					zap.Float64("rate", rate),
					zap.Int64("processed", tickProcessed))

				lastProcesed = output.Processed
				lastTick = t

			case <-timeoutChannel:

				totalWrites := output.Processed - dw.input.Import.Processed

				logger.Warn("data import lambda duration expired", zap.Int64("writes", totalWrites))
				// close down ticker and exit
				ticker.Stop()
				return

			default:

				writestart := time.Now()

				result, writeErr := svc.BatchWriteItemWithContext(dw.ctx, &dynamodb.BatchWriteItemInput{
					RequestItems: map[string][]*dynamodb.WriteRequest{
						dw.input.NewTableName: writeRequests,
					},
				})

				if writeErr != nil {
					if awsErr, ok := writeErr.(awserr.Error); ok {
						// process SDK error
						switch awsErr.Code() {
						case dynamodb.ErrCodeProvisionedThroughputExceededException, dynamodb.ErrCodeRequestLimitExceeded, "ThrottlingException":

							logger.Warn("thoughput error backing off", zap.Int64("itemcount", output.Processed), zap.Error(writeErr))

							// need to sleep when re-requesting, per spec
							if err := aws.SleepWithContext(dw.ctx, boff.NextBackOff()); err != nil {
								logger.Panic("timed out", zap.Error(err))
							}
						default:
							logger.Panic("unknown dynamodb error", zap.Error(writeErr))
						}

					} else {
						logger.Panic("unknown error", zap.Error(writeErr))
					}
				}

				unprocessedWrites := result.UnprocessedItems[dw.input.NewTableName]

				logger.Debug("write completed",
					zap.Int64("writetime", time.Now().Sub(writestart).Milliseconds()),
					zap.Int("items", len(records)),
					zap.Int("unprocessed", len(unprocessedWrites)))

				if len(unprocessedWrites) == 0 {

					// reset backoff
					boff.Reset()

					// add to tally
					output.Processed += int64(len(records))

					// onto next batch
					continue nextbatch

				} else {

					logger.Info("partial write detected",
						zap.Int64("writetime", time.Now().Sub(writestart).Milliseconds()),
						zap.Int("items", len(records)),
						zap.Int("unprocessed", len(unprocessedWrites)))

					// process any remaining writes first
					// ( will be a short write i.e. less then the 25 items we *could* do)
					writeRequests = unprocessedWrites

				}

			}

		}
	}

	ticker.Stop()

	output.Complete = true

	return
}

// Run executes a batch batch.
func (dw *DataWriter) Run() (output state.ImportResult, err error) {
	return dw.dynamodbImport()
}

// Handler is foo
func Handler(ctx context.Context, input state.Schema) (output state.ImportResult, err error) {

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

	logger.Info("dyanmodb data export handler")

	// default to a 25 items write (max allowed)
	// https://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_BatchWriteItem.html
	//
	if input.ImportConfig.BatchSize < 1 {
		input.ImportConfig.BatchSize = 25
	}

	// check for unconfigured state
	if input.Import.Records == "" {
		logger.Panic("no data record passed to process")
	}

	start := time.Now()

	writer := DataWriter{
		input: input,
		ctx:   rqCtx,
	}

	output, err = writer.Run()

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
