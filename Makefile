# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
BINARY_NAME=dynamodb-clone

VERSION=1.0.0

# source database
SOURCEDB ?= ddbimport

# destination database
DESTDB ?= ddbimport-new

COMMIT=$(shell git rev-list -1 HEAD --abbrev-commit)
DATE=$(shell date -u '+%Y%m%d')

all: test build_dataexport build_dataimport build_schemaexport build_schemaimport

deps:
	go get -v  ./...
build_dataexport:
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/data-export -v ./table/data-export

test_dataexport: build_dataexport
	sam local invoke "ddbDataExportFunction" --event ./events/config.json

build_dataimport: 
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/data-import -v ./table/data-import

test_dataimport: build_dataimport
	sam local invoke "ddbDataImportFunction" --event ./events/import.json

build_schemaexport: 
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/schema-export -v ./table/schema-export

test_schemaexport: build_schemaexport
	sam local invoke "ddbSchemaExportFunction" --event ./events/config.json

build_schemaimport: 
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/schema-import -v ./table/schema-import

test_schemaimport: build_schemaimport
	sam local invoke "ddbSchemaImportFunction" --event ./events/config.json

deploy: build_dataexport build_dataimport build_schemaexport build_schemaimport
	sam deploy  --no-confirm-changeset --parameter-overrides ParameterKey=sourceTableName,ParameterValue=${SOURCEDB} ParameterKey=destTableName,ParameterValue=${DESTDB} 

destroy:
	aws cloudformation delete-stack --stack-name dynamodb-clone

local_state_start:
	docker run -d -p 8083:8083 --rm --name=stateserver \
		--env AWS_DEFAULT_REGION="eu-west-1" \
		--env AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
		--env AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
		--env AWS_SECURITY_TOKEN="${AWS_SECURITY_TOKEN}" \
		--env LAMBDA_ENDPOINT="http://localhost:3001" \
		amazon/aws-stepfunctions-local

create_state:
	aws stepfunctions --endpoint http://localhost:8083 create-state-machine --definition "{\
	\"Comment\": \"A Hello World example of the Amazon States Language using an AWS Lambda Local function\",\
	\"StartAt\": \"HelloWorld\",\
	\"States\": {\
		\"HelloWorld\": {\
		\"Type\": \"Task\",\
		\"Resource\": \"arn:aws:lambda:eu-west-1:123456789012:function:ddbSchemaExportFunction\",\
		\"End\": true\
		}\
	}\
	}\
	}}" --name "HelloWorld" --role-arn "arn:aws:iam::012345678901:role/DummyRole"

start_state:
	aws stepfunctions --endpoint http://localhost:8083 start-execution --state-machine arn:aws:states:eu-west-1:123456789012:stateMachine:HelloWorld --name test

result_state:
	aws stepfunctions --endpoint http://localhost:8083 describe-execution --execution-arn arn:aws:states:eu-west-1:123456789012:execution:HelloWorld:test

local_state_stop:
	docker stop stateserver

local_lamda_start:
	sam local start-lambda

test: 
		$(GOTEST) -v ./...
clean: 
		$(GOCLEAN)
		rm -f export

