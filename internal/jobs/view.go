package jobs

import "sftp-relay/internal/db"

// View is the wire shape of a job, shared by the REST handlers and the SSE
// stream so a client never has to reconcile two different representations.
type View struct {
	ID               int64  `json:"id"`
	ServerID         int64  `json:"server_id"`
	ServerName       string `json:"server_name"`
	Kind             string `json:"kind"`
	RemotePath       string `json:"remote_path"`
	DestPath         string `json:"dest_path"`
	Status           string `json:"status"`
	TotalBytes       int64  `json:"total_bytes"`
	TransferredBytes int64  `json:"transferred_bytes"`
	SpeedBPS         int64  `json:"speed_bps"`
	ETASeconds       int64  `json:"eta_seconds"`
	Percent          int    `json:"percent"`
	ExitCode         *int64 `json:"exit_code"`
	Error            string `json:"error"`
	CreatedAt        string `json:"created_at"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at"`
}

// ViewOf converts a row. Percent is derived here rather than stored, so it can
// never disagree with the byte counts it is computed from.
func ViewOf(j db.Job) View {
	v := View{
		ID: j.ID, ServerID: j.ServerID, ServerName: j.ServerName, Kind: j.Kind,
		RemotePath: j.RemotePath, DestPath: j.DestPath, Status: j.Status,
		TotalBytes: j.TotalBytes, TransferredBytes: j.TransferredBytes,
		SpeedBPS: j.SpeedBPS, ETASeconds: j.ETASeconds, Error: j.Error,
		CreatedAt: j.CreatedAt,
	}
	if j.ExitCode.Valid {
		code := j.ExitCode.Int64
		v.ExitCode = &code
	}
	if j.StartedAt.Valid {
		v.StartedAt = j.StartedAt.String
	}
	if j.FinishedAt.Valid {
		v.FinishedAt = j.FinishedAt.String
	}
	v.Percent = percent(j.TransferredBytes, j.TotalBytes)
	if Terminal(j.Status) && j.Status == StatusDone {
		v.Percent = 100
	}
	return v
}

func percent(done, total int64) int {
	if total <= 0 {
		return 0
	}
	p := int(done * 100 / total)
	if p > 100 {
		return 100
	}
	return p
}
