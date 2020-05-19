package state

import "github.com/aws/aws-sdk-go/service/dynamodb"

// SchemaResult from the Lambda.
type SchemaResult struct {
	DurationMS int64 `json:"durationms"`
	Complete   bool  `json:"complete"`
}

//
// ImportConfig from the batch data import
//
type ImportConfig struct {
	BatchSize int64 `json:"batchsize"`
}

//
// ImportResult from the batch data import
//
type ImportResult struct {
	Processed  int64  `json:"processed"`
	Records    string `json:"records"`
	DurationMS int64  `json:"durationms"`
	Complete   bool   `json:"complete"`
}

//
// ExportConfig from the batch data export
//
type ExportConfig struct {
	TotalSegments int64 `json:"totalsegments"`
	Segment       int64 `json:"segment"`
	Limit         int64 `json:"limit"`
}

//
// ExportResult from the batch data export
//
type ExportResult struct {
	Processed  int64                               `json:"processed"`
	Records    []string                            `json:"records"`
	LastKey    map[string]*dynamodb.AttributeValue `json:"lastkey"`
	DurationMS int64                               `json:"durationms"`
	Complete   bool                                `json:"complete"`
}

//
// Schema for the Exporters
//
type Schema struct {
	Region        string       `json:"region"`
	Bucket        string       `json:"bucket"`
	OrigTableName string       `json:"origtable"`
	NewTableName  string       `json:"newtable"`
	Import        ImportResult `json:"dataimporter"`
	Export        ExportResult `json:"dataexporter"`
	ImportConfig  ImportConfig `json:"dataimporterconfig"`
	ExportConfig  ExportConfig `json:"dataexporterconfig"`
}
