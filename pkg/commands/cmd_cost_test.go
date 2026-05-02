package commands

import (
	"context"
	"strings"
	"testing"
)

func TestCostCommand_Definition(t *testing.T) {
	def := costCommand()
	if def.Name != "cost" {
		t.Errorf("name: got %q, want %q", def.Name, "cost")
	}
	if def.Description == "" {
		t.Error("description should not be empty")
	}
	if def.Usage != "/cost" {
		t.Errorf("usage: got %q, want %q", def.Usage, "/cost")
	}
}

func TestCostCommand_NoRuntime(t *testing.T) {
	def := costCommand()
	req := Request{Text: "/cost", Reply: func(string) error { return nil }}
	err := def.Handler(context.Background(), req, nil)
	if err != nil {
		t.Errorf("should not error with nil runtime: %v", err)
	}
}

func TestCostCommand_NoGetCostBreakdown(t *testing.T) {
	def := costCommand()
	req := Request{Text: "/cost", Reply: func(string) error { return nil }}
	rt := &Runtime{}
	err := def.Handler(context.Background(), req, rt)
	if err != nil {
		t.Errorf("should not error with nil GetCostBreakdown: %v", err)
	}
}

func TestCostCommand_WithBreakdown(t *testing.T) {
	def := costCommand()
	var captured string
	req := Request{
		Text:  "/cost",
		Reply: func(msg string) error { captured = msg; return nil },
	}
	rt := &Runtime{
		GetCostBreakdown: func() string {
			return "Session Cost Breakdown\n  Total cost: $0.000420"
		},
	}
	err := def.Handler(context.Background(), req, rt)
	if err != nil {
		t.Errorf("should not error: %v", err)
	}
	if !strings.Contains(captured, "Session Cost Breakdown") {
		t.Errorf("should contain cost breakdown, got: %s", captured)
	}
}

func TestCostCommand_EmptyBreakdown(t *testing.T) {
	def := costCommand()
	var captured string
	req := Request{
		Text:  "/cost",
		Reply: func(msg string) error { captured = msg; return nil },
	}
	rt := &Runtime{
		GetCostBreakdown: func() string { return "" },
	}
	err := def.Handler(context.Background(), req, rt)
	if err != nil {
		t.Errorf("should not error: %v", err)
	}
	if captured != "" {
		t.Errorf("empty breakdown should reply with empty string, got: %q", captured)
	}
}

func TestBuiltinDefinitions_ContainsCost(t *testing.T) {
	defs := BuiltinDefinitions()
	found := false
	for _, def := range defs {
		if def.Name == "cost" {
			found = true
			break
		}
	}
	if !found {
		t.Error("BuiltinDefinitions should include /cost command")
	}
}
