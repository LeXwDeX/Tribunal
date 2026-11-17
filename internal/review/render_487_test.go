package review

import (
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
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
	out, err := RenderPositionThread([]Group{group}, gitlab.Position{}, FindingRenderOptions{Generation: 2})
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
	out, err := RenderPositionThread([]Group{first, second}, gitlab.Position{}, FindingRenderOptions{})
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
	reversed, err := RenderPositionThread([]Group{second, first}, gitlab.Position{}, FindingRenderOptions{})
	if err != nil || reversed != out {
		t.Fatalf("section order is not stable: err=%v\n%s\n%s", err, out, reversed)
	}
}

func TestRenderPositionThreadRejectsMixedPositions(t *testing.T) {
	first := positionGroup("first problem", "one", SeverityP1)
	second := positionGroup("second problem", "two", SeverityP2)
	second.Line = 9
	if _, err := RenderPositionThread([]Group{first, second}, gitlab.Position{}, FindingRenderOptions{}); err == nil {
		t.Fatal("mixed positions rendered into one thread")
	}
}

func TestRenderFindingWithoutGenerationKeepsLegacyBody(t *testing.T) {
	group := positionGroup("bad assignment", "the assignment is wrong", SeverityP2)
	group.PublishKey = PublishKeyFor("main.go", "bad assignment", "")
	out, err := renderFindingForTest(group, FindingRenderOptions{})
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

func TestNormalizeAnchorPositionCollapsesContextLines(t *testing.T) {
	contextAnchor := gitlab.Position{OldPath: "main.go", NewPath: "main.go", OldLine: 4, NewLine: 4}
	if path, line, side := NormalizeAnchorPosition("main.go", 4, SideOld, contextAnchor); path != "main.go" || line != 4 || side != SideNew {
		t.Fatalf("context old side = (%q, %d, %q), want (main.go, 4, new)", path, line, side)
	}
	// Context lines keep both numbers even when the sides are numbered
	// differently; they are still one physical line.
	shifted := gitlab.Position{OldPath: "main.go", NewPath: "main.go", OldLine: 4, NewLine: 7}
	if path, line, side := NormalizeAnchorPosition("main.go", 4, SideOld, shifted); path != "main.go" || line != 7 || side != SideNew {
		t.Fatalf("shifted context old side = (%q, %d, %q), want (main.go, 7, new)", path, line, side)
	}
	if path, line, side := NormalizeAnchorPosition("main.go", 7, SideNew, shifted); path != "main.go" || line != 7 || side != SideNew {
		t.Fatalf("shifted context new side = (%q, %d, %q), want (main.go, 7, new)", path, line, side)
	}
	// A renamed context line collapses onto the new path as well.
	renamed := gitlab.Position{OldPath: "old.go", NewPath: "new.go", OldLine: 4, NewLine: 7}
	if path, line, side := NormalizeAnchorPosition("old.go", 4, SideOld, renamed); path != "new.go" || line != 7 || side != SideNew {
		t.Fatalf("renamed context old side = (%q, %d, %q), want (new.go, 7, new)", path, line, side)
	}
	// Added and deleted lines carry a single number and keep their side.
	added := gitlab.Position{NewPath: "main.go", NewLine: 9}
	if path, line, side := NormalizeAnchorPosition("main.go", 9, SideNew, added); path != "main.go" || line != 9 || side != SideNew {
		t.Fatalf("added line = (%q, %d, %q), want (main.go, 9, new)", path, line, side)
	}
	deleted := gitlab.Position{OldPath: "main.go", OldLine: 9}
	if path, line, side := NormalizeAnchorPosition("main.go", 9, SideOld, deleted); path != "main.go" || line != 9 || side != SideOld {
		t.Fatalf("deleted line = (%q, %d, %q), want (main.go, 9, old)", path, line, side)
	}
}

func TestPositionAnchorKeyMergesContextSides(t *testing.T) {
	anchor := gitlab.Position{OldPath: "main.go", NewPath: "main.go", OldLine: 4, NewLine: 4}
	oldKey := PositionAnchorKey("main.go", 4, SideOld, anchor)
	newKey := PositionAnchorKey("main.go", 4, SideNew, anchor)
	if oldKey != newKey {
		t.Fatalf("context line sides own different keys: %q != %q", oldKey, newKey)
	}
	if oldKey != PositionPublishKey("main.go", 4, SideNew) {
		t.Fatalf("context line key = %q, want the new-side key", oldKey)
	}
	added := gitlab.Position{NewPath: "main.go", NewLine: 4}
	deleted := gitlab.Position{OldPath: "main.go", OldLine: 4}
	if PositionAnchorKey("main.go", 4, SideNew, added) == PositionAnchorKey("main.go", 4, SideOld, deleted) {
		t.Fatal("added and deleted lines share a position key")
	}
}

func TestRenderPositionThreadMergesContextSides(t *testing.T) {
	anchor := gitlab.Position{OldPath: "main.go", NewPath: "main.go", OldLine: 4, NewLine: 4}
	oldSide := positionGroup("first problem", "one", SeverityP1)
	oldSide.Line, oldSide.Side = 4, SideOld
	newSide := positionGroup("second problem", "two", SeverityP2)
	newSide.Line, newSide.Side = 4, SideNew
	out, err := RenderPositionThread([]Group{oldSide, newSide}, anchor, FindingRenderOptions{})
	if err != nil {
		t.Fatalf("RenderPositionThread: %v", err)
	}
	key := PositionPublishKey("main.go", 4, SideNew)
	if !strings.HasPrefix(out, FindingMarkerPrefix+" key="+key+" -->") {
		t.Fatalf("context thread marker = %q, want the new-side key", out)
	}
	if !strings.Contains(out, "first problem") || !strings.Contains(out, "second problem") {
		t.Fatalf("context thread lost one side: %q", out)
	}
}
