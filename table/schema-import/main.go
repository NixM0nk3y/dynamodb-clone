package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/NixM0nk3y/dynamodb-clone/log"
	"github.com/NixM0nk3y/dynamodb-clone/state"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-xray-sdk-go/xray"
	"go.uber.org/zap"
)

// SchemaWriter is a
type SchemaWriter struct {
	input state.Schema
	ctx   context.Context
	err   error
}

func (sw *SchemaWriter) retrieveSchema() (tableSchema map[string]interface{}, err error) {

	logger := log.Logger(sw.ctx)

	logger.Info(fmt.Sprintf("retrieving schema for %s", sw.input.OrigTableName))

	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(sw.input.Region),
	})

	if err != nil {
		return nil, err
	}

	fileName := fmt.Sprintf("%v/schema.json", sw.input.OrigTableName)

	s3Svc := s3.New(sess)
	xray.AWS(s3Svc.Client)

	// Create s3 Client
	downLoader := s3manager.NewDownloaderWithClient(s3Svc)

	w := &aws.WriteAtBuffer{}

	_, downloadErr := downLoader.DownloadWithContext(sw.ctx, w, &s3.GetObjectInput{
		Bucket: aws.String(sw.input.Bucket),
		Key:    aws.String(fileName),
	})

	if downloadErr != nil {
		logger.Panic(fmt.Sprintf("unable to download %s from %s", fileName, sw.input.Bucket), zap.Error(downloadErr))
	}

	//
	errJSON := json.Unmarshal(w.Bytes(), &tableSchema)

	if errJSON != nil {
		logger.Panic("unable to unmarshal record from JSON", zap.Error(errJSON))
	}

	logger.Info(fmt.Sprintf("Successfully retrieved %s from %s", fileName, sw.input.Bucket))

	if _, ok := tableSchema["Table"]; !ok {
		return nil, errors.New("unknown table schema")
	}

	return
}

//
// Only support basic base schema currently
//
func (sw *SchemaWriter) buildDynamodbSchema(tableSchema map[string]interface{}) (ddTable *dynamodb.CreateTableInput) {

	logger := log.Logger(sw.ctx)

	ddTable = &dynamodb.CreateTableInput{
		TableName: aws.String(sw.input.NewTableName),
	}

	table := tableSchema["Table"].(map[string]interface{})

	var provisionMode bool = false
	var provisionReads int64 = 0
	var provisionWrites int64 = 0

	for key, value := range table {

		switch key {

		case "ProvisionedThroughput":

			for tpKey, tpValue := range value.(map[string]interface{}) {

				switch tpKey {

				case "ReadCapacityUnits":
					provisionReads = int64(tpValue.(float64))
				case "WriteCapacityUnits":
					provisionWrites = int64(tpValue.(float64))

				}
			}

		case "BillingModeSummary":

			billingmode := value.(map[string]interface{})

			mode := billingmode["BillingMode"].(string)

			if mode == "PROVISIONED" {
				logger.Warn("warning provisioned throughput may slow down restore")
				provisionMode = true
			}

			ddTable.SetBillingMode(mode)

		case "KeySchema":

			var keySchemaElements []*dynamodb.KeySchemaElement

			for _, attributemap := range value.([]interface{}) {

				attribute := attributemap.(map[string]interface{})

				aname := attribute["AttributeName"].(string)
				ktype := attribute["KeyType"].(string)

				keySchemaElements = append(keySchemaElements, &dynamodb.KeySchemaElement{
					AttributeName: aws.String(aname),
					KeyType:       aws.String(ktype),
				})
			}

			ddTable.SetKeySchema(keySchemaElements)

		case "AttributeDefinitions":

			var attributedefinitions []*dynamodb.AttributeDefinition

			for _, attributemap := range value.([]interface{}) {

				attribute := attributemap.(map[string]interface{})

				aname := attribute["AttributeName"].(string)
				atype := attribute["AttributeType"].(string)

				attributedefinitions = append(attributedefinitions, &dynamodb.AttributeDefinition{
					AttributeName: aws.String(aname),
					AttributeType: aws.String(atype),
				})
			}

			ddTable.SetAttributeDefinitions(attributedefinitions)

		default:
			logger.Debug(fmt.Sprintf("skipping property %s", key))
		}
	}

	if provisionMode {
		provisionSettings := &dynamodb.ProvisionedThroughput{
			ReadCapacityUnits:  aws.Int64(provisionReads),
			WriteCapacityUnits: aws.Int64(provisionWrites),
		}

		ddTable.SetProvisionedThroughput(provisionSettings)
	}

	return
}

//
func (sw *SchemaWriter) dynamodbSchemaImport() (result bool, err error) {

	logger := log.Logger(sw.ctx)

	sess, err := session.NewSession(&aws.Config{
		Region:     aws.String(sw.input.Region),
		MaxRetries: aws.Int(5),
		//LogLevel:   aws.LogLevel(aws.LogDebugWithHTTPBody),
	})

	if err != nil {
		return false, err
	}

	// Create DynamoDB client
	svc := dynamodb.New(sess)

	xray.AWS(svc.Client)

	logger.Info("pulling table schema from storage")

	tableSchema, retrieveErr := sw.retrieveSchema()

	if retrieveErr != nil {
		return false, retrieveErr
	}

	logger.Info(fmt.Sprintf("creating table %s with retrieved schema", sw.input.NewTableName))

	tableInput := sw.buildDynamodbSchema(tableSchema)

	createStart := time.Now()

	_, createError := svc.CreateTableWithContext(sw.ctx, tableInput)
	if createError != nil {
		if aerr, ok := createError.(awserr.Error); ok {
			switch aerr.Code() {
			case dynamodb.ErrCodeResourceNotFoundException:
				logger.Panic("table not found", zap.Error(createError))
			case dynamodb.ErrCodeResourceInUseException:
				logger.Panic("table already exists", zap.Error(createError))
			default:
				logger.Panic("dynamodb returned error", zap.Error(createError))
			}
		} else {
			logger.Panic("unknown error", zap.Error(createError))
		}
		return false, createError
	}

	waitErr := svc.WaitUntilTableExistsWithContext(sw.ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(sw.input.NewTableName),
	})

	if waitErr != nil {
		logger.Panic("failed to wait for table to be created", zap.Error(waitErr))
	} else {
		result = true
	}

	logger.Info("create completed",
		zap.Int64("createtime", time.Now().Sub(createStart).Milliseconds()))

	return
}

// Run executes a import of the schema.
func (sw *SchemaWriter) Run() (complete bool, err error) {
	return sw.dynamodbSchemaImport()
}

// Handler is foo
func Handler(ctx context.Context, input state.Schema) (output state.SchemaResult, err error) {

	lc, _ := lambdacontext.FromContext(ctx)

	rqCtx := log.WithRqID(ctx, lc.AwsRequestID)

	logger := log.Logger(rqCtx).With(zap.String("region", input.Region),
		zap.String("bucket", input.Bucket),
		zap.String("stable", input.OrigTableName),
		zap.String("dtable", input.NewTableName),
	)

	xray.SetLogger(&log.XrayLogger{})

	xray.Configure(xray.Config{
		LogLevel:       "info", // default
		ServiceVersion: "1.2.3",
	})

	logger.Info("dynamodb table schema import")

	var exportError error

	writer := SchemaWriter{
		input: input,
		ctx:   rqCtx,
	}

	start := time.Now()

	output.Complete, exportError = writer.Run()

	if exportError != nil {
		logger.Panic("schema import failed", zap.Error(err))
	}

	output.DurationMS = time.Now().Sub(start).Milliseconds()

	logger.Info("complete", zap.Int64("duration", output.DurationMS))

	return

}

func main() {
	lambda.Start(Handler)
}
