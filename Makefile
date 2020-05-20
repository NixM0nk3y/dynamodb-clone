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

dataexport/test: build_dataexport
	sam local invoke "ddbDataExportFunction" --event ./events/config.json

dataimport/build: 
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/data-import -v ./table/data-import

dataimport/test: build_dataimport
	sam local invoke "ddbDataImportFunction" --event ./events/import.json

schemaexport/build: 
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/schema-export -v ./table/schema-export

schemaexport/test: build_schemaexport
	sam local invoke "ddbSchemaExportFunction" --event ./events/config.json

schemaimport/build: 
	$(GOBUILD) -ldflags " \
		-X github.com/NixM0nk3y/dynamodb-clone/version.Version=${VERSION} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildHash=${COMMIT} \
		-X github.com/NixM0nk3y/dynamodb-clone/version.BuildDate=${DATE}" \
		-o ./bin/schema-import -v ./table/schema-import

schemaimport/test: build_schemaimport
	sam local invoke "ddbSchemaImportFunction" --event ./events/config.json

clone/deploy: build_dataexport build_dataimport build_schemaexport build_schemaimport
	sam deploy  --no-confirm-changeset --s3-bucket=${SAMBUCKET} --parameter-overrides ParameterKey=sourceTableName,ParameterValue=${SOURCEDB} ParameterKey=destTableName,ParameterValue=${DESTDB} 

clone/run:
	$(eval CLONEBUCKET=$(shell aws cloudformation describe-stack-resources --stack-name dynamodb-clone | jq -rc '.StackResources[] | select( .ResourceType == "AWS::S3::Bucket" )| .PhysicalResourceId'))
	$(eval STATEMACHINE=$(shell aws cloudformation describe-stack-resources --stack-name dynamodb-clone | jq -rc '.StackResources[] | select( .ResourceType == "AWS::StepFunctions::StateMachine" )| .PhysicalResourceId'))

	aws stepfunctions start-execution --state-machine ${STATEMACHINE} --input '{ "region": "eu-west-1", "bucket": "${CLONEBUCKET}", "origtable": "${SOURCEDB}", "newtable": "${DESTDB}" }'

clone/destroy:
	aws cloudformation delete-stack --stack-name dynamodb-clone

test/stateserver/start:
	docker run -d -p 8083:8083 --rm --name=stateserver \
		--env AWS_DEFAULT_REGION="eu-west-1" \
		--env AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
		--env AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
		--env AWS_SECURITY_TOKEN="${AWS_SECURITY_TOKEN}" \
		--env LAMBDA_ENDPOINT="http://host.docker.internal:3001" \
		amazon/aws-stepfunctions-local

test/stateserver/stop:
	docker stop stateserver

test/dynamodb/start:
	docker run -d -p 8000:8000 --rm --name=dynamodb \
		amazon/dynamodb-local

test/dynamodb/stop:
	docker stop dynamodb

test/state/create:

	yq -r .Resources.ddbCloneStateMachine.Properties.DefinitionString[0] template.yaml > /tmp/state.json
	sed -i 's/$${SchemaImportArn}/arn:aws:lambda:eu-west-1:123456789012:function:ddbSchemaImportFunction/g' /tmp/state.json
	sed -i 's/$${SchemaExportArn}/arn:aws:lambda:eu-west-1:123456789012:function:ddbSchemaExportFunction/g' /tmp/state.json
	sed -i 's/$${DataImportArn}/arn:aws:lambda:eu-west-1:123456789012:function:ddbDataImportFunction/g' /tmp/state.json
	sed -i 's/$${DataExportArn}/arn:aws:lambda:eu-west-1:123456789012:function:ddbDataExportFunction/g' /tmp/state.json

	aws stepfunctions --endpoint http://localhost:8083 create-state-machine --definition '$(shell cat /tmp/state.json)' --name "ddbClone" --role-arn "arn:aws:iam::012345678901:role/DummyRole"

test/state/start:
	aws stepfunctions --endpoint http://localhost:8083 start-execution --state-machine arn:aws:states:eu-west-1:123456789012:stateMachine:ddbClone --input '{ "region": "eu-west-1", "bucket": "${TESTBUCKET}", "origtable": "${SOURCEDB}", "newtable": "${DESTDB}" }' --name test

test/state/result:
	aws stepfunctions --endpoint http://localhost:8083 describe-execution --execution-arn arn:aws:states:eu-west-1:123456789012:execution:ddbClone:test

test/lamda/start:
	sam local start-lambda

test: 
		$(GOTEST) -v ./...
clean: 
		$(GOCLEAN)
		rm -f ./bin/*

