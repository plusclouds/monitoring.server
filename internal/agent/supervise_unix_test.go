//go:build unix

package agent

import (
	"testing"
	"time"
)

func TestProcessExitCode(t *testing.T) {
	p, err := StartProcess([]string{"sh", "-c", "exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("no exit")
	}
	if p.ExitCode() != 3 {
		t.Errorf("code %d", p.ExitCode())
	}
	if o := p.Object(); o.Status != Critical || o.Metrics["up"] != 0 {
		t.Errorf("object %+v", o)
	}
}

func TestProcessRunningObject(t *testing.T) {
	p, err := StartProcess([]string{"sleep", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if o := p.Object(); o.Metrics["up"] != 1 || o.Status != "" {
		t.Errorf("object %+v", o)
	}
}

func TestStartMissingCommand(t *testing.T) {
	if _, err := StartProcess([]string{"/nonexistent/x"}); err == nil {
		t.Error("started")
	}
	if _, err := StartProcess(nil); err == nil {
		t.Error("empty accepted")
	}
}
