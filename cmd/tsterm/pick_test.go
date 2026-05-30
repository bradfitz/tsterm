package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/bradfitz/tsterm/pkg/proto"
)

func summary(id, name string, active bool) proto.SessionSummary {
	return proto.SessionSummary{ID: id, Name: name, Active: active, Rows: 24, Cols: 80}
}

func TestPickEmptyCreates(t *testing.T) {
	p, err := pickSession(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.AttachID != "" || p.CreateName != "" {
		t.Errorf("pick = %+v, want empty CreateName", p)
	}
}

func TestPickSoleAttaches(t *testing.T) {
	p, err := pickSession([]proto.SessionSummary{summary("id1", "foo", true)}, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.AttachID != "id1" {
		t.Errorf("pick = %+v, want AttachID id1", p)
	}
}

func TestPickAmbiguousErrors(t *testing.T) {
	_, err := pickSession([]proto.SessionSummary{
		summary("id1", "foo", true),
		summary("id2", "bar", true),
	}, "")
	if !errors.Is(err, ErrAmbiguous) {
		t.Errorf("err = %v, want ErrAmbiguous", err)
	}
}

func TestPickInactiveIgnored(t *testing.T) {
	// Only one *active* session; inactive ones should not contribute.
	p, err := pickSession([]proto.SessionSummary{
		summary("id1", "foo", false),
		summary("id2", "bar", true),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.AttachID != "id2" {
		t.Errorf("pick = %+v, want AttachID id2", p)
	}
}

func TestPickByName(t *testing.T) {
	p, err := pickSession([]proto.SessionSummary{
		summary("id1", "build", true),
		summary("id2", "logs", true),
	}, "logs")
	if err != nil {
		t.Fatal(err)
	}
	if p.AttachID != "id2" {
		t.Errorf("pick = %+v, want AttachID id2", p)
	}
}

func TestPickByNameMissing(t *testing.T) {
	_, err := pickSession([]proto.SessionSummary{summary("id1", "x", true)}, "y")
	if err == nil || strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("err = %v, want a not-found-style error", err)
	}
}
