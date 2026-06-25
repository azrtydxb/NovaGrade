package contracts

// Envelope is the message wrapper used across queue and HTTP boundaries.
// It carries multi-tenancy fields (TenantID, Principal) alongside routing
// and correlation metadata. Every command or result event is transported
// inside an Envelope with a PayloadRef pointing to the object-store key
// that holds the actual payload.
type Envelope struct {
	TenantID      string `json:"tenant_id"`
	Principal     string `json:"principal"`
	SubmissionID  string `json:"submission_id"`
	BatchID       string `json:"batch_id"`
	Stage         string `json:"stage"`
	Attempt       int    `json:"attempt"`
	CorrelationID string `json:"correlation_id"`
	PayloadRef    string `json:"payload_ref"`
}

// Command event types — used in the Stage field to indicate the work to perform.
const (
	StageSubmitExam string = "submit_exam"
	StageRender     string = "render"
	StageTranscribe string = "transcribe"
	StageGrade      string = "grade"
	StagePublish    string = "publish"
	StageExport     string = "export"
)

// Result event types — used in the Stage field to indicate a completed outcome.
const (
	StageRenderResult     string = "render.result"
	StageTranscribeResult string = "transcribe.result"
	StageGradeResult      string = "grade.result"
	StagePublishResult    string = "publish.result"
	StageExportResult     string = "export.result"
)
