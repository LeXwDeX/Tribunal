package review

import (
	"strings"
	"testing"
)

func TestPositionPublishKeyUsesOnlyTheDiffAnchor(t *testing.T) {
	key := PositionPublishKey("main.go", 7, SideNew)
	if !publishKeyRE.MatchString(key) {
		t.Fatalf("position key %q is not a valid pk_ key", key)
	}
	if again := PositionPublishKey("main.go", 7, SideNew); again != key {
		t.Fatalf("position key is not stable: %q != %q", again, key)
	}
	if other := PositionPublishKey("main.go", 8, SideNew); other == key {
		t.Fatal("different lines share a position key")
	}
	if other := PositionPublishKey("other.go", 7, SideNew); other == key {
		t.Fatal("different files share a position key")
	}
	if other := PositionPublishKey("main.go", 7, SideOld); other == key {
		t.Fatal("different sides share a position key")
	}
	if empty := PositionPublishKey("", 7, SideNew); empty != "" {
		t.Fatalf("empty path produced %q", empty)
	}
	if empty := PositionPublishKey("main.go", 0, SideNew); empty != "" {
		t.Fatalf("missing line produced %q", empty)
	}
}

func positionGroup(title, body string, severity Severity) Group {
	return Group{
		ID: "g-" + NormalizeTitle(title), Path: "main.go", Line: 7, Side: SideNew,
		Severity: severity, Title: title, Body: body, PositionOK: true,
		Decision: &FindingDecision{Action: ActionPublish},
	}
}

func TestRenderPositionThreadMarksGenerationAndKeepsFlatBody(t *testing.T) {
	group := positionGroup("bad assignment", "the assignment is wrong", SeverityP2)
	out, err := RenderPositionThread([]Group{group}, FindingRenderOptions{Generation: 2})
	if err != nil {
		t.Fatalf("RenderPositionThread: %v", err)
	}
	key := PositionPublishKey("main.go", 7, SideNew)
	if !strings.HasPrefix(out, FindingMarkerPrefix+" key="+key+" -->") {
		t.Fatalf("thread does not start with the position marker: %q", out)
	}
	if !strings.Contains(out, "代次 2") {
		t.Fatalf("thread lacks the generation marker: %q", out)
	}
	if !strings.Contains(out, "**[P2]bad assignment**") {
		t.Fatalf("single finding lost the flat title: %q", out)
	}
}

func TestRenderPositionThreadSeparatesProblemsAtOnePosition(t *testing.T) {
	first := positionGroup("second problem", "the body hides an error", SeverityP2)
	second := positionGroup("first problem", "the body is wrong", SeverityP1)
	out, err := RenderPositionThread([]Group{first, second}, FindingRenderOptions{})
	if err != nil {
		t.Fatalf("RenderPositionThread: %v", err)
	}
	if strings.Count(out, "\n### ") != 2 {
		t.Fatalf("problems were not rendered as sections: %q", out)
	}
	if strings.Index(out, "[P1]first problem") > strings.Index(out, "[P2]second problem") {
		t.Fatalf("sections are not ordered by severity: %q", out)
	}
	// Stable order: the same groups in reverse input order render identically.
	reversed, err := RenderPositionThread([]Group{second, first}, FindingRenderOptions{})
	if err != nil || reversed != out {
		t.Fatalf("section order is not stable: err=%v\n%s\n%s", err, out, reversed)
	}
}

func TestRenderPositionThreadRejectsMixedPositions(t *testing.T) {
	first := positionGroup("first problem", "one", SeverityP1)
	second := positionGroup("second problem", "two", SeverityP2)
	second.Line = 9
	if _, err := RenderPositionThread([]Group{first, second}, FindingRenderOptions{}); err == nil {
		t.Fatal("mixed positions rendered into one thread")
	}
}

func TestRenderFindingWithoutGenerationKeepsLegacyBody(t *testing.T) {
	group := positionGroup("bad assignment", "the assignment is wrong", SeverityP2)
	group.PublishKey = PublishKeyFor("main.go", "bad assignment", "")
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatalf("RenderFinding: %v", err)
	}
	if strings.Contains(out, "代次") {
		t.Fatalf("legacy finding render gained a generation marker: %q", out)
	}
	if !strings.Contains(out, "**[P2]bad assignment**") || !strings.Contains(out, "the assignment is wrong") {
		t.Fatalf("legacy finding render changed: %q", out)
	}
}
