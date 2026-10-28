package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/skill/builtin"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// ExtensionDefault is one built-in platform extension: Tribunal registers it
// once, keeps it current with the running release, and grants it to review
// blocks by default. Kind and Name form the ledger key, so they must stay
// stable across releases. Skills and MCP servers use the same seeder; only
// the callbacks differ.
type ExtensionDefault struct {
	Kind string
	Name string
	// Exists reports an administrator-owned platform extension with the same
	// name. It is never replaced, and the default is then not bound either.
	Exists func(ctx context.Context) (bool, error)
	// Install creates the built-in row. It must be a no-op when the built-in
	// row already exists, so an interrupted first start can be retried.
	Install func(ctx context.Context) error
	// Sync, when set, runs on every start after registration and brings the
	// built-in row up to the running release. It must keep the row's
	// enabled state.
	Sync func(ctx context.Context) error
	// Bindings maps a block to the entries this default adds to it.
	Bindings map[string]DefaultGrant
}

// DefaultGrant is what one default adds to a platform block binding.
type DefaultGrant struct {
	MCPTools []string
	Skills   []string
}

const (
	reasonExtensionExists = "a platform extension with this name already exists"
	reasonNotRegistered   = "the default extension was not registered"
	reasonFileBinding     = "the block is bound in the configuration file"
	reasonCustomBinding   = "the block binding was changed by an administrator"
)

// ApplyExtensionDefaults registers and binds platform defaults. Every
// decision is recorded in the extension_defaults ledger and taken once:
//
//   - "<kind>:<name>": registered, or skipped because a platform extension of
//     that name already exists.
//   - "<kind>:<name>@<block>": granted on the block, or skipped because the
//     configuration file binds the block or an administrator owns its row.
//   - "block:<block>": the binding the defaults last wrote. A block row that
//     still matches it is default-owned and may receive later defaults. The
//     row and its ledger rows are written in one transaction.
//
// When the configuration file declares a block whose row is still
// default-owned, the row is removed so the file binding takes effect; the
// snapshot is then recorded as skipped. A row an administrator changed keeps
// winning over the file.
//
// Unbinding a default therefore survives restarts, and upgrades never rewrite
// a customised binding. The built-in row itself is refreshed by Sync on every
// start; its enabled state belongs to the administrator. A failed install is
// not recorded and is retried on the next start.
func ApplyExtensionDefaults(ctx context.Context, st *sqlite.Store, fileBlocks map[string]config.BlockBinding,
	defaults []ExtensionDefault, logger *slog.Logger) error {
	if len(defaults) == 0 && len(fileBlocks) == 0 {
		return nil
	}
	if st == nil {
		return errors.New("app: default extension store is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, d := range defaults {
		if err := validateExtensionDefault(d); err != nil {
			return err
		}
	}
	rows, err := st.ListExtensionDefaults(ctx)
	if err != nil {
		return err
	}
	ledger := make(map[string]store.ExtensionDefault, len(rows))
	for _, row := range rows {
		ledger[row.Key] = row
	}
	seeder := &defaultSeeder{st: st, fileBlocks: fileBlocks, ledger: ledger, logger: logger}
	if err := seeder.releaseFileBlocks(ctx); err != nil {
		return err
	}
	for _, d := range defaults {
		if err := seeder.apply(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func validateExtensionDefault(d ExtensionDefault) error {
	if strings.TrimSpace(d.Kind) == "" || strings.TrimSpace(d.Name) == "" {
		return errors.New("app: default extension kind and name are required")
	}
	if d.Exists == nil || d.Install == nil {
		return fmt.Errorf("app: default %s %q needs Exists and Install", d.Kind, d.Name)
	}
	for block := range d.Bindings {
		if strings.TrimSpace(block) == "" {
			return fmt.Errorf("app: default %s %q has an empty block name", d.Kind, d.Name)
		}
	}
	return nil
}

type defaultSeeder struct {
	st         *sqlite.Store
	fileBlocks map[string]config.BlockBinding
	ledger     map[string]store.ExtensionDefault
	logger     *slog.Logger
}

func (s *defaultSeeder) apply(ctx context.Context, d ExtensionDefault) error {
	key := d.Kind + ":" + d.Name
	if _, done := s.ledger[key]; !done {
		exists, err := d.Exists(ctx)
		if err != nil {
			return fmt.Errorf("app: check default %s %q: %w", d.Kind, d.Name, err)
		}
		if exists {
			if err := s.record(ctx, key, store.ExtensionDefaultSkipped, reasonExtensionExists); err != nil {
				return err
			}
		} else {
			if err := d.Install(ctx); err != nil {
				return fmt.Errorf("app: install default %s %q: %w", d.Kind, d.Name, err)
			}
			if err := s.record(ctx, key, store.ExtensionDefaultApplied, ""); err != nil {
				return err
			}
		}
	}
	registered := s.ledger[key].Outcome == store.ExtensionDefaultApplied
	if registered && d.Sync != nil {
		if err := d.Sync(ctx); err != nil {
			return fmt.Errorf("app: update default %s %q: %w", d.Kind, d.Name, err)
		}
	}
	blocks := make([]string, 0, len(d.Bindings))
	for block := range d.Bindings {
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)
	for _, block := range blocks {
		grantKey := key + "@" + block
		want, err := encodeDefaultGrant(d.Bindings[block])
		if err != nil {
			return err
		}
		if done, ok := s.ledger[grantKey]; ok {
			// A release may change what a default grants. An applied grant
			// follows it while the block row is still default-owned; the
			// entries of the previous grant are replaced.
			if done.Outcome != store.ExtensionDefaultApplied || !registered || done.Detail == want {
				continue
			}
			merged, _, err := s.grant(ctx, block, d.Bindings[block], decodeDefaultGrant(done.Detail))
			if err != nil {
				return err
			}
			if merged != nil {
				if err := s.commitGrant(ctx, *merged, grantKey, want); err != nil {
					return err
				}
			}
			continue
		}
		if !registered {
			if err := s.record(ctx, grantKey, store.ExtensionDefaultSkipped, reasonNotRegistered); err != nil {
				return err
			}
			continue
		}
		merged, reason, err := s.grant(ctx, block, d.Bindings[block], DefaultGrant{})
		if err != nil {
			return err
		}
		if merged != nil {
			if err := s.commitGrant(ctx, *merged, grantKey, want); err != nil {
				return err
			}
			continue
		}
		s.logger.Info("default extension binding skipped", "kind", d.Kind, "name", d.Name, "block", block, "reason", reason)
		if err := s.record(ctx, grantKey, store.ExtensionDefaultSkipped, reason); err != nil {
			return err
		}
	}
	return nil
}

// encodeDefaultGrant is the ledger detail of an applied grant: the entries it
// added, so a later release can replace exactly those.
func encodeDefaultGrant(grant DefaultGrant) (string, error) {
	data, err := json.Marshal(defaultBindingSnapshot{
		MCPTools: append([]string{}, grant.MCPTools...),
		Skills:   append([]string{}, grant.Skills...),
	})
	if err != nil {
		return "", fmt.Errorf("app: encode default grant: %w", err)
	}
	return string(data), nil
}

// decodeDefaultGrant reads an applied grant's detail. Grants recorded before
// details were kept decode as empty, so an upgrade only adds entries.
func decodeDefaultGrant(detail string) DefaultGrant {
	var snapshot defaultBindingSnapshot
	if json.Unmarshal([]byte(detail), &snapshot) != nil {
		return DefaultGrant{}
	}
	return DefaultGrant(snapshot)
}

// grant merges one default grant into a default-owned platform binding,
// first dropping the entries of the previous grant it replaces, and returns
// the binding to write. It returns nil and the reason when the block is not
// default-owned. Defaults must not grant overlapping entries: replacing one
// default's grant would also drop an entry another default shares. MCP tools
// carry their server name, and each skill default grants only its own name.
func (s *defaultSeeder) grant(ctx context.Context, block string, grant, previous DefaultGrant) (*store.BlockBinding, string, error) {
	if _, ok := s.fileBlocks[block]; ok {
		return nil, reasonFileBinding, nil
	}
	current, err := s.platformBinding(ctx, block)
	if err != nil {
		return nil, "", err
	}
	snapshot, owned := s.ledger[blockSnapshotKey(block)]
	switch {
	case current == nil && owned:
		return nil, reasonCustomBinding, nil
	case current != nil && (!owned || !sameDefaultBinding(*current, snapshot.Detail)):
		return nil, reasonCustomBinding, nil
	}
	merged := store.BlockBinding{Scope: store.ScopePlatform, Block: block}
	if current != nil {
		merged.MCPTools = withoutValues(current.MCPTools, previous.MCPTools)
		merged.Skills = withoutValues(current.Skills, previous.Skills)
	}
	merged.MCPTools = appendMissing(merged.MCPTools, grant.MCPTools)
	merged.Skills = appendMissing(merged.Skills, grant.Skills)
	return &merged, "", nil
}

// commitGrant writes a merged default-owned binding together with its block
// snapshot and the grant's ledger row in one transaction, so a failure never
// leaves a default-written row that a later start takes for an
// administrator's change.
func (s *defaultSeeder) commitGrant(ctx context.Context, merged store.BlockBinding, grantKey, want string) error {
	detail, err := encodeDefaultBinding(merged)
	if err != nil {
		return err
	}
	rows := []store.ExtensionDefault{
		{Key: blockSnapshotKey(merged.Block), Outcome: store.ExtensionDefaultApplied, Detail: detail},
		{Key: grantKey, Outcome: store.ExtensionDefaultApplied, Detail: want},
	}
	if err := s.st.SetDefaultBlockBinding(ctx, merged.Block, &merged, rows...); err != nil {
		return fmt.Errorf("app: write default block binding %q: %w", merged.Block, err)
	}
	for _, row := range rows {
		s.ledger[row.Key] = row
	}
	return nil
}

// releaseFileBlocks removes the default-owned platform row of every block the
// configuration file declares, so a row the defaults wrote before the block
// was configured does not shadow the file binding. Rows an administrator
// changed are kept. The removal and its ledger row are one transaction, and
// the snapshot is recorded as skipped so the release happens once.
func (s *defaultSeeder) releaseFileBlocks(ctx context.Context) error {
	blocks := make([]string, 0, len(s.fileBlocks))
	for block := range s.fileBlocks {
		if snapshot, ok := s.ledger[blockSnapshotKey(block)]; ok && snapshot.Outcome == store.ExtensionDefaultApplied {
			blocks = append(blocks, block)
		}
	}
	sort.Strings(blocks)
	for _, block := range blocks {
		current, err := s.platformBinding(ctx, block)
		if err != nil {
			return err
		}
		if current == nil || !sameDefaultBinding(*current, s.ledger[blockSnapshotKey(block)].Detail) {
			continue
		}
		row := store.ExtensionDefault{Key: blockSnapshotKey(block), Outcome: store.ExtensionDefaultSkipped, Detail: reasonFileBinding}
		if err := s.st.SetDefaultBlockBinding(ctx, block, nil, row); err != nil {
			return fmt.Errorf("app: release default block binding %q: %w", block, err)
		}
		s.ledger[row.Key] = row
		s.logger.Info("file configuration replaces default block binding", "block", block)
	}
	return nil
}

func (s *defaultSeeder) platformBinding(ctx context.Context, block string) (*store.BlockBinding, error) {
	rows, err := s.st.ListBlockBindings(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, fmt.Errorf("app: load platform block bindings: %w", err)
	}
	for i := range rows {
		if rows[i].Block == block {
			return &rows[i], nil
		}
	}
	return nil, nil
}

func blockSnapshotKey(block string) string {
	return "block:" + block
}

func (s *defaultSeeder) record(ctx context.Context, key, outcome, detail string) error {
	row := store.ExtensionDefault{Key: key, Outcome: outcome, Detail: detail}
	if err := s.st.PutExtensionDefault(ctx, row); err != nil {
		return err
	}
	s.ledger[key] = row
	return nil
}

type defaultBindingSnapshot struct {
	MCPTools []string `json:"mcp_tools"`
	Skills   []string `json:"skills"`
}

func encodeDefaultBinding(binding store.BlockBinding) (string, error) {
	snapshot := defaultBindingSnapshot{
		MCPTools: append([]string{}, binding.MCPTools...),
		Skills:   append([]string{}, binding.Skills...),
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("app: encode default block binding: %w", err)
	}
	return string(data), nil
}

func sameDefaultBinding(binding store.BlockBinding, detail string) bool {
	var snapshot defaultBindingSnapshot
	if json.Unmarshal([]byte(detail), &snapshot) != nil {
		return false
	}
	return reflect.DeepEqual(append([]string{}, binding.MCPTools...), append([]string{}, snapshot.MCPTools...)) &&
		reflect.DeepEqual(append([]string{}, binding.Skills...), append([]string{}, snapshot.Skills...))
}

func withoutValues(values, drop []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !containsString(drop, value) {
			out = append(out, value)
		}
	}
	return out
}

func appendMissing(values, add []string) []string {
	for _, value := range add {
		if !containsString(values, value) {
			values = append(values, value)
		}
	}
	return values
}

// SkillDefaults turns the configured bundled skills into extension defaults.
// Install copies the embedded skill into the managed platform skills root, so
// the row is listed and loaded like an uploaded skill; it is marked built-in,
// can be disabled and cannot be deleted. Sync refreshes the installed files
// and description when the release ships different content.
func (s *ExtensionService) SkillDefaults(specs []config.DefaultSkill) ([]ExtensionDefault, error) {
	out := make([]ExtensionDefault, 0, len(specs))
	for _, spec := range specs {
		name := spec.Name
		if !builtin.Has(name) {
			return nil, fmt.Errorf("app: unknown bundled skill %q", name)
		}
		bindings := make(map[string]DefaultGrant, len(spec.Blocks))
		for _, block := range spec.Blocks {
			bindings[block] = DefaultGrant{Skills: []string{name}}
		}
		out = append(out, ExtensionDefault{
			Kind: "skill",
			Name: name,
			Exists: func(ctx context.Context) (bool, error) {
				row, err := s.platformSkillNamed(ctx, name)
				return row != nil && row.Builtin == "", err
			},
			Install: func(ctx context.Context) error {
				row, err := s.platformSkillNamed(ctx, name)
				if err != nil || row != nil {
					return err
				}
				_, err = s.installBuiltinSkill(ctx, name)
				return err
			},
			Sync: func(ctx context.Context) error {
				row, err := s.platformSkillNamed(ctx, name)
				if err != nil || row == nil || row.Builtin != name {
					return err
				}
				// A refresh failure, such as a row left outside the managed
				// root after workspace.root moved, must not stop the service.
				// The installed copy stays as it is and loads, or fails to
				// load, like any other skill.
				if err := s.refreshBuiltinSkill(ctx, row); err != nil {
					s.logger.Warn("built-in skill refresh failed", "name", name, "error_class", errclass.Of(err))
				}
				return nil
			},
			Bindings: bindings,
		})
	}
	return out, nil
}

func (s *ExtensionService) platformSkillNamed(ctx context.Context, name string) (*store.Skill, error) {
	rows, err := s.store.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// installBuiltinSkill stages the embedded files below the platform skills
// root and persists them through the same path as an upload.
func (s *ExtensionService) installBuiltinSkill(ctx context.Context, name string) (*Skill, error) {
	scopeRoot, err := s.skillScopeRoot(store.ScopePlatform, nil)
	if err != nil {
		return nil, err
	}
	staged, description, err := stageBuiltinSkill(name, scopeRoot)
	if err != nil {
		return nil, err
	}
	return s.persistSkillDir(ctx, store.ScopePlatform, nil, name, description, name, scopeRoot, staged)
}

// refreshBuiltinSkill replaces the installed files when they differ from the
// embedded release copy. The row, its path and its enabled state are kept.
func (s *ExtensionService) refreshBuiltinSkill(ctx context.Context, row *store.Skill) error {
	container, managed := s.managedSkillContainer(row)
	if !managed {
		return fmt.Errorf("app: built-in skill %q is outside the managed skills root", row.Name)
	}
	files, err := builtin.Sub(row.Builtin)
	if err != nil {
		return err
	}
	current, err := sameSkillFiles(files, row.Dir)
	if err != nil {
		return err
	}
	if !current {
		staged, _, err := stageBuiltinSkill(row.Builtin, container)
		if err != nil {
			return err
		}
		retired := filepath.Join(container, fmt.Sprintf(".retired-%d", s.now().UnixNano()))
		if err := os.Rename(row.Dir, retired); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.RemoveAll(staged)
			return fmt.Errorf("app: replace built-in skill %q: %w", row.Name, err)
		}
		if err := os.Rename(staged, row.Dir); err != nil {
			_ = os.Rename(retired, row.Dir)
			_ = os.RemoveAll(staged)
			return fmt.Errorf("app: replace built-in skill %q: %w", row.Name, err)
		}
		if err := os.RemoveAll(retired); err != nil {
			s.logger.Warn("retired built-in skill cleanup failed", "skill_id", row.ID)
		}
		s.logger.Info("built-in skill updated", "name", row.Name)
	}
	_, description, err := readSkillManifest(row.Dir)
	if err != nil {
		return fmt.Errorf("app: built-in skill %q: %w", row.Name, err)
	}
	if description != row.Description {
		if err := s.store.SetSkillDescription(ctx, row.ID, description); err != nil {
			return err
		}
	}
	return nil
}

// stageBuiltinSkill copies one embedded skill into a new directory below
// parent and returns it with the manifest description.
func stageBuiltinSkill(name, parent string) (string, string, error) {
	files, err := builtin.Sub(name)
	if err != nil {
		return "", "", err
	}
	staged, err := os.MkdirTemp(parent, ".builtin-*")
	if err != nil {
		return "", "", fmt.Errorf("app: create skill directory: %w", err)
	}
	if err := copyEmbeddedSkill(files, staged); err != nil {
		_ = os.RemoveAll(staged)
		return "", "", err
	}
	manifestName, description, err := readSkillManifest(staged)
	if err != nil || manifestName != name {
		_ = os.RemoveAll(staged)
		return "", "", fmt.Errorf("app: bundled skill %q has an invalid manifest", name)
	}
	return staged, description, nil
}

// sameSkillFiles reports whether dir holds exactly the embedded files.
func sameSkillFiles(files fs.FS, dir string) (bool, error) {
	want := make(map[string][]byte)
	if err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(files, name)
		want[name] = data
		return err
	}); err != nil {
		return false, err
	}
	same := true
	err := filepath.WalkDir(dir, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			same = false
			return fs.SkipAll
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, current)
		if err != nil {
			return err
		}
		expected, ok := want[filepath.ToSlash(rel)]
		if !ok || !entry.Type().IsRegular() {
			same = false
			return fs.SkipAll
		}
		data, err := os.ReadFile(current)
		if err != nil || !bytes.Equal(data, expected) {
			same = false
			return fs.SkipAll
		}
		delete(want, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return false, err
	}
	return same && len(want) == 0, nil
}

func copyEmbeddedSkill(files fs.FS, dst string) error {
	return fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(path.Clean(name)))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}

// seedDefaultExtensions applies the configured defaults before the runtime
// reads stored platform extensions, so the first start already uses them.
func seedDefaultExtensions(ctx context.Context, cfg *config.Config, st *sqlite.Store, manager *workspace.Manager, logger *slog.Logger, now func() time.Time) error {
	if len(cfg.DefaultExtensions.Skills) == 0 && len(cfg.DefaultExtensions.MCPServers) == 0 {
		// Still release default-owned rows of blocks the file now declares.
		if err := ApplyExtensionDefaults(ctx, st, cfg.Blocks, nil, logger); err != nil {
			return fmt.Errorf("app: apply default extensions: %w", err)
		}
		return nil
	}
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:     st,
		Workspace: manager,
		DeniedEnv: deniedEnvNames(cfg),
		Logger:    logger,
		Now:       now,
	})
	if err != nil {
		return err
	}
	defaults, err := service.SkillDefaults(cfg.DefaultExtensions.Skills)
	if err != nil {
		return err
	}
	fileMCP, err := loadPlatformMCP(cfg)
	if err != nil {
		return err
	}
	servers, err := service.MCPDefaults(cfg.DefaultExtensions.MCPServers, fileMCP)
	if err != nil {
		return err
	}
	defaults = append(defaults, servers...)
	if err := ApplyExtensionDefaults(ctx, st, cfg.Blocks, defaults, logger); err != nil {
		return fmt.Errorf("app: apply default extensions: %w", err)
	}
	return nil
}
