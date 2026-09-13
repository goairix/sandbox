package metrics

import (
	"context"
	"testing"
)

func BenchmarkWorkspaceStageNoop(b *testing.B) {
	if err := InitNoop(); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		RecordWorkspaceStage(ctx, "kubernetes", "minio", "readiness", "success", 0.01)
	}
}
