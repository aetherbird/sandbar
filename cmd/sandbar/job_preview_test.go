package main

import "testing"

func TestSupervisedJobResultPreview(t *testing.T) {
	tests := []struct {
		name string
		tool string
		raw  string
		want string
	}{
		{
			name: "async shell start", tool: "shell_exec",
			raw:  `{"job_id":"job_123456789abcdef","state":"running"}`,
			want: "job job_12345678 · running",
		},
		{
			name: "completed job tail", tool: "job",
			raw:  `{"job_id":"job_short","state":"completed","stdout_tail":"first\nsecond"}`,
			want: "job job_short · completed · first second",
		},
		{
			name: "timed out wait", tool: "job",
			raw:  `{"job_id":"job_short","state":"running","wait_timed_out":true}`,
			want: "job job_short · running · wait timed out",
		},
		{
			name: "job list", tool: "job",
			raw:  `{"jobs":[{"job_id":"one","state":"running"},{"job_id":"two","state":"completed"}]}`,
			want: "2 background jobs · 1 running",
		},
		{
			name: "empty job list", tool: "job",
			raw:  `{"jobs":[]}`,
			want: "no background jobs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := supervisedJobResultPreview(tc.tool, tc.raw)
			if !ok || got != tc.want {
				t.Fatalf("preview = %q, %v; want %q, true", got, ok, tc.want)
			}
			if rendered := toolResultPreview(tc.tool, tc.raw); rendered != tc.want {
				t.Fatalf("tool result preview = %q, want %q", rendered, tc.want)
			}
		})
	}
}

func TestSupervisedJobResultPreviewRejectsOrdinaryResults(t *testing.T) {
	for _, tc := range []struct{ tool, raw string }{
		{"shell_exec", "Exit code: 0\nStdout:\nok"},
		{"job", `{"message":"not a snapshot"}`},
		{"file_read", `{"job_id":"job_1","state":"running"}`},
	} {
		if got, ok := supervisedJobResultPreview(tc.tool, tc.raw); ok || got != "" {
			t.Fatalf("preview(%q, %q) = %q, %v; want rejection", tc.tool, tc.raw, got, ok)
		}
	}
}
