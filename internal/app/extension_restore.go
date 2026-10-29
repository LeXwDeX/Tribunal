package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

const (
	restoreStatusChange    = "change"
	restoreStatusUnchanged = "unchanged"
	restoreStatusSkipped   = "skipped"

	restoreReasonFileBinding      = "file_binding"
	restoreReasonNoRegistered     = "no_registered_defaults"
	restoreMCPToolPrefix          = "mcp__"
	restoreManagedKindMCP         = "mcp"
	restoreConflictMessage        = "the default block bindings changed since the preview; preview again"
	restorePlatformConfigRequired = "app: platform configuration is required to restore defaults"
)

// RestoreDefaultsRequest restores the shipped grants of selected blocks.
// PreviewToken is the fingerprint returned by a dry run. An empty Blocks list
// means every bindable block.
type RestoreDefaultsRequest struct {
	Blocks       []string
	DryRun       bool
	PreviewToken string
}

// RestoreDefaultsChange is one block's preview or execution outcome. Add and
// Remove are the entries the restore adds to or removes from the current row;
// they are empty for unchanged and skipped blocks. OwnershipChanged reports
// whether the restore creates or rewrites the platform row or its block
// snapshot and thereby makes it default-owned. A pure grant ledger update is
// not an ownership change.
type RestoreDefaultsChange struct {
	Block            string       `json:"block"`
	Status           string       `json:"status"`
	Reason           string       `json:"reason"`
	Add              DefaultGrant `json:"add"`
	Remove           DefaultGrant `json:"remove"`
	OwnershipChanged bool         `json:"ownership_changed"`
}

// RestoreDefaultsResult is the preview and execution response. RestartRequired
// is always true: the runtime reads platform bindings at start.
type RestoreDefaultsResult struct {
	Changes         []RestoreDefaultsChange `json:"changes"`
	PreviewToken    string                  `json:"preview_token"`
	RestartRequired bool                    `json:"restart_required"`
}

// RestoreDefaults previews or applies the shipped defaults to platform block
// bindings. It trusts only the configuration loaded by this process and the
// stored rows read inside the write transaction; request grants are never
// accepted. A preview writes nothing. Execution writes every changed block
// and ledger row in one transaction and fails with a conflict when the
// fingerprint moved.
func (s *ExtensionService) RestoreDefaults(ctx context.Context, request RestoreDefaultsRequest) (*RestoreDefaultsResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("app: extension store is required")
	}
	defaults, fileBlocks, err := s.restoreDefaultSet()
	if err != nil {
		return nil, err
	}
	blocks, err := normalizeRestoreBlocks(request.Blocks)
	if err != nil {
		return nil, err
	}
	if request.DryRun {
		state, err := s.readRestoreState(ctx)
		if err != nil {
			return nil, err
		}
		outcome, err := planDefaultRestore(defaults, fileBlocks, blocks, state)
		if err != nil {
			return nil, err
		}
		return outcome.result, nil
	}
	if strings.TrimSpace(request.PreviewToken) == "" {
		return nil, validationError("preview_token is required when dry_run is false",
			Detail{Field: "preview_token", Message: "required"})
	}
	var result *RestoreDefaultsResult
	if _, err := s.store.RestoreDefaultBlockBindings(ctx, func(state sqlite.DefaultRestoreState) (sqlite.DefaultRestorePlan, error) {
		outcome, err := planDefaultRestore(defaults, fileBlocks, blocks, state)
		if err != nil {
			return sqlite.DefaultRestorePlan{}, err
		}
		if outcome.result.PreviewToken != request.PreviewToken {
			return sqlite.DefaultRestorePlan{}, conflictError(restoreConflictMessage)
		}
		result = outcome.result
		return outcome.plan, nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// restoreDefaultSet builds the defaults and file blocks from the process
// configuration once. The running process never re-reads configuration files
// for a restore, so a preview and its execution compare the same defaults.
func (s *ExtensionService) restoreDefaultSet() ([]ExtensionDefault, map[string]config.BlockBinding, error) {
	s.restoreOnce.Do(func() {
		if s.platform == nil {
			s.restoreErr = errors.New(restorePlatformConfigRequired)
			return
		}
		skills, err := s.SkillDefaults(s.platform.DefaultExtensions.Skills)
		if err != nil {
			s.restoreErr = err
			return
		}
		// Restore only needs the shipped grant declarations. An empty file MCP
		// config avoids reading mcp.config again; the declared defaults do not
		// change with that file.
		servers, err := s.MCPDefaults(s.platform.DefaultExtensions.MCPServers, mcp.Config{})
		if err != nil {
			s.restoreErr = err
			return
		}
		s.restoreSet = append(skills, servers...)
		s.restoreFile = make(map[string]config.BlockBinding, len(s.platform.Blocks))
		for block, binding := range s.platform.Blocks {
			s.restoreFile[block] = config.BlockBinding{
				MCPTools: append([]string(nil), binding.MCPTools...),
				Skills:   append([]string(nil), binding.Skills...),
			}
		}
	})
	return s.restoreSet, s.restoreFile, s.restoreErr
}

func (s *ExtensionService) readRestoreState(ctx context.Context) (sqlite.DefaultRestoreState, error) {
	return s.store.ReadDefaultRestoreState(ctx)
}

func normalizeRestoreBlocks(requested []string) ([]string, error) {
	allowed := make(map[string]bool, len(config.BindableBlocks))
	for _, block := range config.BindableBlocks {
		allowed[block] = true
	}
	if len(requested) == 0 {
		requested = config.BindableBlocks
	}
	seen := make(map[string]bool, len(requested))
	blocks := make([]string, 0, len(requested))
	for _, candidate := range requested {
		block := strings.TrimSpace(candidate)
		if !allowed[block] {
			return nil, validationError("blocks must contain only bindable block names",
				Detail{Field: "blocks", Message: "unknown block " + block})
		}
		if seen[block] {
			continue
		}
		seen[block] = true
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)
	return blocks, nil
}

type restoreOutcome struct {
	result *RestoreDefaultsResult
	plan   sqlite.DefaultRestorePlan
}

func planDefaultRestore(defaults []ExtensionDefault, fileBlocks map[string]config.BlockBinding,
	blocks []string, state sqlite.DefaultRestoreState) (*restoreOutcome, error) {
	bindings := make(map[string]store.BlockBinding, len(state.Bindings))
	for _, binding := range state.Bindings {
		bindings[binding.Block] = binding
	}
	ledger := make(map[string]store.ExtensionDefault, len(state.Ledger))
	for _, row := range state.Ledger {
		ledger[row.Key] = row
	}
	builtins := make(map[string]sqlite.BuiltinExtension, len(state.Builtins))
	for _, row := range state.Builtins {
		builtins[row.Kind+":"+row.Name] = row
	}
	result := &RestoreDefaultsResult{
		Changes:         make([]RestoreDefaultsChange, 0, len(blocks)),
		RestartRequired: true,
	}
	outcome := &restoreOutcome{result: result}
	for _, block := range blocks {
		change, binding, rows, err := planRestoreBlock(block, defaults, fileBlocks, bindings, ledger, builtins)
		if err != nil {
			return nil, err
		}
		result.Changes = append(result.Changes, change)
		if binding != nil {
			outcome.plan.Bindings = append(outcome.plan.Bindings, *binding)
		}
		outcome.plan.Ledger = append(outcome.plan.Ledger, rows...)
	}
	token, err := restorePreviewToken(blocks, fileBlocks, defaults, state)
	if err != nil {
		return nil, err
	}
	result.PreviewToken = token
	return outcome, nil
}

func planRestoreBlock(block string, defaults []ExtensionDefault, fileBlocks map[string]config.BlockBinding,
	bindings map[string]store.BlockBinding, ledger map[string]store.ExtensionDefault,
	builtins map[string]sqlite.BuiltinExtension) (RestoreDefaultsChange, *store.BlockBinding, []store.ExtensionDefault, error) {
	skipped := func(reason string) (RestoreDefaultsChange, *store.BlockBinding, []store.ExtensionDefault, error) {
		return newRestoreChange(block, restoreStatusSkipped, reason, DefaultGrant{}, DefaultGrant{}, false), nil, nil, nil
	}
	if _, ok := fileBlocks[block]; ok {
		return skipped(restoreReasonFileBinding)
	}
	registered := make([]ExtensionDefault, 0, len(defaults))
	blockDefaults := make([]ExtensionDefault, 0, len(defaults))
	for _, d := range defaults {
		key := d.Kind + ":" + d.Name
		if ledger[key].Outcome != store.ExtensionDefaultApplied {
			continue
		}
		builtin, ok := builtins[key]
		if !ok || builtin.Builtin != d.Name {
			continue
		}
		registered = append(registered, d)
		if _, granted := d.Bindings[block]; granted {
			blockDefaults = append(blockDefaults, d)
		}
	}
	if len(registered) == 0 {
		return skipped(restoreReasonNoRegistered)
	}

	current, exists := bindings[block]
	desired := store.BlockBinding{Scope: store.ScopePlatform, Block: block, MCPTools: []string{}, Skills: []string{}}
	if exists {
		desired.MCPTools = append(desired.MCPTools, current.MCPTools...)
		desired.Skills = append(desired.Skills, current.Skills...)
	}
	// The restore scope is every registered built-in, not only the defaults
	// that still grant this block. Built-in grants an administrator added to a
	// block the defaults do not declare must go away too; custom and unknown
	// grants stay.
	//
	// The runtime trims Skill names and MCP tool names before it binds them,
	// so restore applies the same rule. Only entries that normalize to this
	// built-in go away; custom, unknown and blank entries keep their exact
	// stored text and order.
	for _, d := range registered {
		if d.Kind == restoreManagedKindMCP {
			desired.MCPTools = withoutMCPToolsOfServer(desired.MCPTools, d.Name)
			continue
		}
		desired.Skills = withoutNormalizedValue(desired.Skills, d.Name)
	}
	for _, d := range blockDefaults {
		grant := d.Bindings[block]
		desired.MCPTools = appendMissing(desired.MCPTools, grant.MCPTools)
		desired.Skills = appendMissing(desired.Skills, grant.Skills)
	}

	// A membership test cannot describe duplicate entries. Compare the exact
	// stored strings as a multiset so a preview lists the same entries the
	// write adds or removes, including repeated and whitespace-padded names.
	add := DefaultGrant{
		MCPTools: missingEntries(desired.MCPTools, current.MCPTools),
		Skills:   missingEntries(desired.Skills, current.Skills),
	}
	remove := DefaultGrant{
		MCPTools: missingEntries(current.MCPTools, desired.MCPTools),
		Skills:   missingEntries(current.Skills, desired.Skills),
	}
	contentChanged := exists && (!sameStringSet(current.MCPTools, desired.MCPTools) || !sameStringSet(current.Skills, desired.Skills))
	if exists && !contentChanged {
		// The entries are unchanged. Keep the stored order so the snapshot
		// compares equal to the row; the upgrader compares ordered slices.
		desired.MCPTools = append([]string{}, current.MCPTools...)
		desired.Skills = append([]string{}, current.Skills...)
	}

	blockKey := blockSnapshotKey(block)
	blockDetail, err := encodeDefaultBinding(desired)
	if err != nil {
		return RestoreDefaultsChange{}, nil, nil, err
	}
	nonEmpty := len(desired.MCPTools) > 0 || len(desired.Skills) > 0
	blockRow, hasBlockRow := ledger[blockKey]
	// A block with no row, no shipped grant and no old snapshot is a true
	// no-op. Do not create a new empty row: that would change how a later
	// file binding is released.
	blockSnapshotNeedsWrite := (exists || nonEmpty || hasBlockRow) &&
		(!hasBlockRow || blockRow.Outcome != store.ExtensionDefaultApplied || blockRow.Detail != blockDetail)
	rows := make([]store.ExtensionDefault, 0, len(blockDefaults)+1)
	grantLedgerChanged := false
	for _, d := range blockDefaults {
		grantKey := d.Kind + ":" + d.Name + "@" + block
		want, err := encodeDefaultGrant(d.Bindings[block])
		if err != nil {
			return RestoreDefaultsChange{}, nil, nil, err
		}
		row, ok := ledger[grantKey]
		if ok && row.Outcome == store.ExtensionDefaultApplied && row.Detail == want {
			continue
		}
		grantLedgerChanged = true
		rows = append(rows, store.ExtensionDefault{Key: grantKey, Outcome: store.ExtensionDefaultApplied, Detail: want})
	}
	// A grant the current release does not declare for this block is marked
	// removed, so a later release that grants it again follows the normal
	// re-add path. A ledger row that never existed needs no marker.
	removedLedgerChanged := false
	for _, d := range registered {
		if _, kept := d.Bindings[block]; kept {
			continue
		}
		grantKey := d.Kind + ":" + d.Name + "@" + block
		row, ok := ledger[grantKey]
		if !ok || (row.Outcome == store.ExtensionDefaultSkipped && row.Detail == reasonGrantRemoved) {
			continue
		}
		removedLedgerChanged = true
		rows = append(rows, store.ExtensionDefault{Key: grantKey, Outcome: store.ExtensionDefaultSkipped, Detail: reasonGrantRemoved})
	}
	// A ledger-only change must not create a binding. A new binding, in turn,
	// always gets its block snapshot: otherwise the next grant looks
	// administrator-owned and cannot follow the defaults.
	bindingWrite := (exists && contentChanged) || (!exists && (nonEmpty || hasBlockRow))
	if bindingWrite && !blockSnapshotNeedsWrite && !hasBlockRow {
		blockSnapshotNeedsWrite = true
	}
	ledgerChanged := blockSnapshotNeedsWrite || grantLedgerChanged || removedLedgerChanged
	if blockSnapshotNeedsWrite {
		rows = append(rows, store.ExtensionDefault{Key: blockKey, Outcome: store.ExtensionDefaultApplied, Detail: blockDetail})
	}
	if !bindingWrite && !ledgerChanged {
		return newRestoreChange(block, restoreStatusUnchanged, "", DefaultGrant{}, DefaultGrant{}, false), nil, nil, nil
	}
	owned := exists && hasBlockRow && blockRow.Outcome == store.ExtensionDefaultApplied &&
		sameDefaultBinding(current, blockRow.Detail)
	ownershipChanged := !owned && (bindingWrite || blockSnapshotNeedsWrite)
	change := newRestoreChange(block, restoreStatusChange, "", add, remove, ownershipChanged)
	var binding *store.BlockBinding
	if bindingWrite {
		binding = &desired
	}
	return change, binding, rows, nil
}

func newRestoreChange(block, status, reason string, add, remove DefaultGrant, ownershipChanged bool) RestoreDefaultsChange {
	if add.MCPTools == nil {
		add.MCPTools = []string{}
	}
	if add.Skills == nil {
		add.Skills = []string{}
	}
	if remove.MCPTools == nil {
		remove.MCPTools = []string{}
	}
	if remove.Skills == nil {
		remove.Skills = []string{}
	}
	return RestoreDefaultsChange{
		Block: block, Status: status, Reason: reason,
		Add: add, Remove: remove, OwnershipChanged: ownershipChanged,
	}
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, value := range a {
		counts[value]++
	}
	for _, value := range b {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	return true
}

// missingEntries returns the entries of values that remain after removing the
// exact strings in exclude by multiplicity. The result is sorted for the
// response contract; duplicate entries are preserved.
func missingEntries(values, exclude []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	remaining := make(map[string]int, len(exclude))
	for _, value := range exclude {
		remaining[value]++
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if remaining[value] > 0 {
			remaining[value]--
			continue
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// withoutNormalizedValue drops every entry the runtime would bind under name,
// after the same TrimSpace normalization the Skill loader applies. Other
// entries keep their original text.
func withoutNormalizedValue(values []string, name string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != name {
			out = append(out, value)
		}
	}
	return out
}

// withoutMCPToolsOfServer drops every entry the runtime would parse as one of
// the server's tools. splitToolName trims the whole entry, so the stored text
// may carry leading or trailing spaces.
func withoutMCPToolsOfServer(values []string, server string) []string {
	prefix := restoreMCPToolPrefix + server + "__"
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !strings.HasPrefix(strings.TrimSpace(value), prefix) {
			out = append(out, value)
		}
	}
	return out
}

type restoreTokenFile struct {
	Block    string   `json:"block"`
	Set      bool     `json:"set"`
	MCPTools []string `json:"mcp_tools"`
	Skills   []string `json:"skills"`
}

type restoreTokenBinding struct {
	Block    string   `json:"block"`
	Exists   bool     `json:"exists"`
	MCPTools []string `json:"mcp_tools"`
	Skills   []string `json:"skills"`
}

type restoreTokenLedger struct {
	Key     string `json:"key"`
	Exists  bool   `json:"exists"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

type restoreTokenBuiltin struct {
	Key     string `json:"key"`
	Exists  bool   `json:"exists"`
	Builtin string `json:"builtin"`
	Enabled bool   `json:"enabled"`
}

type restoreTokenDefaultBlock struct {
	Block    string   `json:"block"`
	Present  bool     `json:"present"`
	MCPTools []string `json:"mcp_tools"`
	Skills   []string `json:"skills"`
}

type restoreTokenDefault struct {
	Kind   string                     `json:"kind"`
	Name   string                     `json:"name"`
	Blocks []restoreTokenDefaultBlock `json:"blocks"`
}

type restoreTokenState struct {
	Blocks   []string              `json:"blocks"`
	File     []restoreTokenFile    `json:"file"`
	Bindings []restoreTokenBinding `json:"bindings"`
	Ledger   []restoreTokenLedger  `json:"ledger"`
	Builtins []restoreTokenBuiltin `json:"builtins"`
	Defaults []restoreTokenDefault `json:"defaults"`
}

// restorePreviewToken fingerprints the state a restore decision depends on:
// the selected blocks, their file declarations, platform rows, related ledger
// rows, the identity and enabled state of every shipped built-in, and the
// shipped grant projection for the selected blocks. Every stored and shipped
// string slice keeps its order: the default snapshot comparison and grant
// upgrade path are order-sensitive, so a reorder must invalidate the preview.
// It is a change detector, not an authorization token.
func restorePreviewToken(blocks []string, fileBlocks map[string]config.BlockBinding,
	defaults []ExtensionDefault, state sqlite.DefaultRestoreState) (string, error) {
	bindings := make(map[string]store.BlockBinding, len(state.Bindings))
	for _, binding := range state.Bindings {
		bindings[binding.Block] = binding
	}
	ledger := make(map[string]store.ExtensionDefault, len(state.Ledger))
	for _, row := range state.Ledger {
		ledger[row.Key] = row
	}
	builtins := make(map[string]sqlite.BuiltinExtension, len(state.Builtins))
	for _, row := range state.Builtins {
		builtins[row.Kind+":"+row.Name] = row
	}
	token := restoreTokenState{Blocks: append([]string(nil), blocks...)}
	for _, block := range blocks {
		file, set := fileBlocks[block]
		token.File = append(token.File, restoreTokenFile{
			Block:    block,
			Set:      set,
			MCPTools: append([]string{}, file.MCPTools...),
			Skills:   append([]string{}, file.Skills...),
		})
		binding, exists := bindings[block]
		token.Bindings = append(token.Bindings, restoreTokenBinding{
			Block:    block,
			Exists:   exists,
			MCPTools: append([]string{}, binding.MCPTools...),
			Skills:   append([]string{}, binding.Skills...),
		})
	}
	keys := make(map[string]bool)
	for _, block := range blocks {
		keys[blockSnapshotKey(block)] = true
	}
	for _, d := range defaults {
		keys[d.Kind+":"+d.Name] = true
		for _, block := range blocks {
			keys[d.Kind+":"+d.Name+"@"+block] = true
		}
	}
	ledgerKeys := make([]string, 0, len(keys))
	for key := range keys {
		ledgerKeys = append(ledgerKeys, key)
	}
	sort.Strings(ledgerKeys)
	for _, key := range ledgerKeys {
		row, exists := ledger[key]
		token.Ledger = append(token.Ledger, restoreTokenLedger{
			Key: key, Exists: exists, Outcome: row.Outcome, Detail: row.Detail,
		})
	}
	builtinKeys := make([]string, 0, len(defaults))
	for _, d := range defaults {
		builtinKeys = append(builtinKeys, d.Kind+":"+d.Name)
	}
	sort.Strings(builtinKeys)
	for _, key := range builtinKeys {
		row, exists := builtins[key]
		token.Builtins = append(token.Builtins, restoreTokenBuiltin{
			Key: key, Exists: exists, Builtin: row.Builtin, Enabled: row.Enabled,
		})
	}
	// The shipped grant projection is part of the fingerprint: another
	// process with the same storage but a different configuration must not
	// accept this preview. Keep the declaration order: planRestoreBlock
	// appends the grants in this order, so reordering defaults changes the
	// stored binding and its snapshot even when the set is equal.
	for _, d := range defaults {
		entry := restoreTokenDefault{Kind: d.Kind, Name: d.Name}
		for _, block := range blocks {
			grant, present := d.Bindings[block]
			entry.Blocks = append(entry.Blocks, restoreTokenDefaultBlock{
				Block:    block,
				Present:  present,
				MCPTools: append([]string{}, grant.MCPTools...),
				Skills:   append([]string{}, grant.Skills...),
			})
		}
		token.Defaults = append(token.Defaults, entry)
	}
	data, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func sortedCopy(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return out
}
