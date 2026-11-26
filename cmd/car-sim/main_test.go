package main

import (
	"os"
	"strings"
	"testing"
)

func TestSavePlaybackReusesConsumedResult(t *testing.T) {
	output := t.TempDir()
	id := "result-" + strings.Repeat("f", 64)
	first, replayed, err := savePlayback(output, id, 1, 2, []byte{1, 2})
	if err != nil || replayed {
		t.Fatalf("first delivery: replayed=%v err=%v", replayed, err)
	}
	second, replayed, err := savePlayback(output, id, 2, 1, []byte{3, 4})
	if err != nil || !replayed || first != second {
		t.Fatalf("replayed result: reused=%v replayed=%v err=%v", first == second, replayed, err)
	}
	pcm, err := os.ReadFile(second)
	if err != nil || string(pcm) != string([]byte{1, 2}) {
		t.Fatalf("previous output was replaced: %v err=%v", pcm, err)
	}
}
