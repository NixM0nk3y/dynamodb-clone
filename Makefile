# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
BINARY_NAME=dynamodb-clone

VERSION=1.0.0

# source database
SAMBUCKET ?= aws-sam-cli-managed-default-samclisourcebucket-1bac5dz9xjbuu

# source database
SOURCEDB ?= ddbimport

# destination database
DESTDB ?= ddbimport-new

# bucket used for test
TESTBUCKET ?= test-bucket

COMMIT=$(shell git rev-list -1 HEAD --abbrev-commit)
DATE=$(shell date -u '+%Y%m%d')

all: test dataimport/build dataexport/build schemaexport/build schemaimport/build

deps:
	go get -v  ./...
dataexport/build:
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/data-export -v ./table/data-export

dataexport/test: dataexport/build
	sam local invoke "ddbDataExportFunction" --event ./events/config.json

dataexport/local/test: dataexport/build
	sam local invoke "ddbDataExportFunction" --event ./test/config.json --env-vars ./test/testenvironment.json

dataimport/build: 
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/data-import -v ./table/data-import

dataimport/test: dataimport/build
	sam local invoke "ddbDataImportFunction" --event ./events/import.json

dataimport/local/test: dataimport/build
	sam local invoke "ddbDataImportFunction" --event ./test/config.json --env-vars ./test/testenvironment.json

schemaexport/build: 
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/schema-export -v ./table/schema-export

schemaexport/test: schemaexport/build
	sam local invoke "ddbSchemaExportFunction" --event ./events/config.json

schemaexport/local/test: schemaexport/build
	sam local invoke "ddbSchemaExportFunction" --event ./test/config.json --env-vars ./test/testenvironment.json

schemaimport/build: 
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/schema-import -v ./table/schema-import

schemaimport/test: schemaimport/build
	sam local invoke "ddbSchemaImportFunction" --event ./events/config.json

schemaimport/local/test: schemaimport/build
	sam local invoke "ddbSchemaImportFunction" --event ./test/config.json --env-vars ./test/testenvironment.json

clone/deploy: dataexport/build dataimport/build schemaexport/build schemaimport/build
	sam deploy  --no-confirm-changeset --s3-bucket=${SAMBUCKET} --parameter-overrides ParameterKey=sourceTableName,ParameterValue=${SOURCEDB} ParameterKey=destTableName,ParameterValue=${DESTDB} 

clone/run:
	$(eval CLONEBUCKET=$(shell aws cloudformation describe-stack-resources --stack-name dynamodb-clone | jq -rc '.StackResources[] | select( .ResourceType == "AWS::S3::Bucket" )| .PhysicalResourceId'))
	$(eval STATEMACHINE=$(shell aws cloudformation describe-stack-resources --stack-name dynamodb-clone | jq -rc '.StackResources[] | select( .ResourceType == "AWS::StepFunctions::StateMachine" )| .PhysicalResourceId'))

	aws stepfunctions start-execution --state-machine ${STATEMACHINE} --input '{ "region": "eu-west-1", "bucket": "${CLONEBUCKET}", "origtable": "${SOURCEDB}", "newtable": "${DESTDB}" }'

clone/destroy:
	aws cloudformation delete-stack --stack-name dynamodb-clone

test/dynamodb/create:
	aws --endpoint-url=http://localhost:4566 \
		dynamodb create-table \
		--table-name ${SOURCEDB} \
		--attribute-definitions AttributeName=Id,AttributeType=N \
		--key-schema AttributeName=Id,KeyType=HASH \
		--billing-mode PAY_PER_REQUEST 

test/dynamodb/load:
	aws --endpoint-url=http://localhost:4566 \
		dynamodb batch-write-item  \
		--request-items file://test/testdata.json
 
test/s3/create:
	aws s3 --endpoint http://localhost:4566 mb s3://${TESTBUCKET}

test/localstack/start:
	docker run -d -p 4566:4566 --rm --name=localstack \
		--env DEFAULT_REGION="eu-west-1" \
		--env FORCE_NONINTERACTIVE="true" \
		--env SKIP_INFRA_DOWNLOADS="true" \
			--env SERVICES="s3,dynamodb,stepfunctions" \
			--end DYNAMODB_ERROR_PROBABILITY="0.1" \
			--env STEPFUNCTIONS_LAMBDA_ENDPOINT="http://host.docker.internal:3001" \
			localstack/localstack-light

test/localstack/stop:
	docker stop localstack

test/state/create:

	yq -r .Resources.ddbCloneStateMachine.Properties.DefinitionString[0] template.yaml > /tmp/state.json
	sed -i 's/$${SchemaImportArn}/arn:aws:lambda:eu-west-1:123456789012:function:ddbSchemaImportFunction/g' /tmp/state.json
	sed -i 's/$${SchemaExportArn}/arn:aws:lambda:eu-west-1:123456789012:function:ddbSchemaExportFunction/g' /tmp/state.json
	sed -i 's/$${DataImportArn}/arn:aws:lambda:eu-west-1:123456789012:function:ddbDataImportFunction/g' /tmp/state.json
	sed -i 's/$${DataExportArn}/arn:aws:lambda:eu-west-1:123456789012:function:ddbDataExportFunction/g' /tmp/state.json

	aws stepfunctions --endpoint http://localhost:4566 create-state-machine --definition '$(shell cat /tmp/state.json)' --name "ddbClone" --role-arn "arn:aws:iam::012345678901:role/DummyRole"

test/state/start:
	aws stepfunctions --endpoint http://localhost:4566 start-execution --state-machine arn:aws:states:eu-west-1:000000000000:stateMachine:ddbClone --input '{ "region": "eu-west-1", "bucket": "${TESTBUCKET}", "origtable": "${SOURCEDB}", "newtable": "${DESTDB}" }' --name test

test/state/result:
	aws stepfunctions --endpoint http://localhost:4566 describe-execution --execution-arn arn:aws:states:eu-west-1:000000000000:execution:ddbClone:test

test/lamda/start:
	sam local start-lambda

test/lamda/local/start:
	sam local start-lambda --env-vars ./test/testenvironment.json

test: 
		$(GOTEST) -v ./...
clean: 
		$(GOCLEAN)
		rm -f ./bin/*

