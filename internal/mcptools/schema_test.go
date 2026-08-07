package mcptools

import (
	"reflect"
	"testing"
)

func TestObjReqShape(t *testing.T) {
	s := objReq(map[string]any{"x": propStr()}, "x")
	if s["type"] != "object" {
		t.Errorf("type = %v, want object", s["type"])
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		t.Fatal("properties is nil")
	}
	if _, ok := props["x"]; !ok {
		t.Error("missing property x")
	}
	req, _ := s["required"].([]string)
	if len(req) != 1 || req[0] != "x" {
		t.Errorf("required = %v, want [x]", req)
	}
}

func TestObjReqEmptyRequired(t *testing.T) {
	s := objReq(map[string]any{"x": propStr()})
	if s["type"] != "object" {
		t.Errorf("type = %v, want object", s["type"])
	}
	req, ok := s["required"].([]string)
	if !ok || len(req) != 0 {
		t.Errorf("required = %v, want []", s["required"])
	}
}

func objSchemaProps(schema map[string]any) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	return props
}

func objSchemaRequired(schema map[string]any) []string {
	if req, ok := schema["required"].([]string); ok {
		return req
	}
	return nil
}

func TestPerTicketSchemasRequired(t *testing.T) {
	tests := []struct {
		name   string
		schema map[string]any
	}{
		{"claimSchema", claimSchema()},
		{"commentSchema", commentSchema()},
		{"getSchema", getSchema()},
		{"completeSchema", completeSchema()},
		{"blockSchema", blockSchema()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := objSchemaRequired(tt.schema)
			if !reflect.DeepEqual(req, []string{"id", "board"}) {
				t.Errorf("required = %v, want [id board]", req)
			}
			props := objSchemaProps(tt.schema)
			if props["id"] == nil {
				t.Error("id property missing")
			}
			if props["board"] == nil {
				t.Error("board property missing")
			}
		})
	}
}

func TestNonPerTicketSchemasNoIDRequired(t *testing.T) {
	boardList := obj(map[string]any{"include_archived": propBool()})
	if r := objSchemaRequired(boardList); r != nil {
		t.Errorf("board_list required = %v, want nil", r)
	}

	kanbanHelp := obj(map[string]any{})
	if r := objSchemaRequired(kanbanHelp); r != nil {
		t.Errorf("kanban_help required = %v, want nil", r)
	}

	ticketList := obj(map[string]any{
		"board":    propStr(),
		"status":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"assignee": propStr(),
		"limit":    propInt(),
	})
	if r := objSchemaRequired(ticketList); r != nil {
		t.Errorf("ticket_list required = %v, want nil", r)
	}

	ticketCreate := obj(map[string]any{
		"board":           propStr(),
		"title":           propStr(),
		"body":            propStr(),
		"assignee":        propStr(),
		"priority":        propInt(),
		"workspace_kind":  propStr(),
		"parents":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"skills":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"triage":          propBool(),
		"idempotency_key": propStr(),
	})
	if r := objSchemaRequired(ticketCreate); r != nil {
		t.Errorf("ticket_create required = %v, want nil", r)
	}
}

func TestClaimSchemaProperties(t *testing.T) {
	props := objSchemaProps(claimSchema())
	if len(props) != 3 {
		t.Errorf("claimSchema has %d properties, want 3", len(props))
	}
	for _, key := range []string{"board", "id", "worker"} {
		if props[key] == nil {
			t.Errorf("claimSchema missing property %q", key)
		}
	}
}

func TestCommentSchemaProperties(t *testing.T) {
	props := objSchemaProps(commentSchema())
	if len(props) != 4 {
		t.Errorf("commentSchema has %d properties, want 4", len(props))
	}
	for _, key := range []string{"board", "id", "body", "author"} {
		if props[key] == nil {
			t.Errorf("commentSchema missing property %q", key)
		}
	}
}

func TestGetSchemaProperties(t *testing.T) {
	props := objSchemaProps(getSchema())
	if len(props) != 2 {
		t.Errorf("getSchema has %d properties, want 2", len(props))
	}
	for _, key := range []string{"board", "id"} {
		if props[key] == nil {
			t.Errorf("getSchema missing property %q", key)
		}
	}
}

func TestCompleteSchemaProperties(t *testing.T) {
	props := objSchemaProps(completeSchema())
	if len(props) != 6 {
		t.Errorf("completeSchema has %d properties, want 6", len(props))
	}
	for _, key := range []string{"board", "id", "summary", "result", "metadata", "review_tier"} {
		if props[key] == nil {
			t.Errorf("completeSchema missing property %q", key)
		}
	}
	rt := props["review_tier"].(map[string]any)
	enum, _ := rt["enum"].([]string)
	if !reflect.DeepEqual(enum, []string{"LOW", "MEDIUM", "HIGH"}) {
		t.Errorf("review_tier enum = %v, want [LOW MEDIUM HIGH]", enum)
	}
}

func TestBlockSchemaProperties(t *testing.T) {
	props := objSchemaProps(blockSchema())
	if len(props) != 4 {
		t.Errorf("blockSchema has %d properties, want 4", len(props))
	}
	for _, key := range []string{"board", "id", "reason", "kind"} {
		if props[key] == nil {
			t.Errorf("blockSchema missing property %q", key)
		}
	}
	k := props["kind"].(map[string]any)
	enum, _ := k["enum"].([]string)
	if !reflect.DeepEqual(enum, []string{"dependency", "needs_input", "capability", "transient"}) {
		t.Errorf("kind enum = %v", enum)
	}
}
