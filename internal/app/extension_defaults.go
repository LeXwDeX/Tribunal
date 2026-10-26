package app

import (
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
	"github.com/LeXwDeX/tribunal/internal/skill/builtin"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// ExtensionDefault is one platform extension that Tribunal registers once
// and grants to review blocks by default. Kind and Name form the ledger key,
// so they must stay stable across releases. Skills and MCP servers use the
// same seeder; only Exists and Install differ.
type ExtensionDefault struct {
	Kind string
	Name string
	// Exists reports a platform extension with the same name. An existing
	// row is never replaced, and the default is then not bound either.
	Exists func(ctx context.Context) (bool, error)
	// Install creates the platform extension row.
	Install func(ctx context.Context) error
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
//     still matches it is default-owned and may receive later defaults.
//
// Deleting or unbinding a default therefore survives restarts, and upgrades
// never rewrite a customised binding. A failed install is not recorded and
// is retried on the next start.
func ApplyExtensionDefaults(ctx context.Context, st *sqlite.Store, fileBlocks map[string]config.BlockBinding,
	defaults []ExtensionDefault, logger *slog.Logger) error {
	if len(defaults) == 0 {
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
	blocks := make([]string, 0, len(d.Bindings))
	for block := range d.Bindings {
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)
	for _, block := range blocks {
		grantKey := key + "@" + block
		if _, done := s.ledger[grantKey]; done {
			continue
		}
		if !registered {
			if err := s.record(ctx, grantKey, store.ExtensionDefaultSkipped, reasonNotRegistered); err != nil {
				return err
			}
			continue
		}
		outcome, reason, err := s.grant(ctx, block, d.Bindings[block])
		if err != nil {
			return err
		}
		if outcome == store.ExtensionDefaultSkipped {
			s.logger.Info("default extension binding skipped", "kind", d.Kind, "name", d.Name, "block", block, "reason", reason)
		}
		if err := s.record(ctx, grantKey, outcome, reason); err != nil {
			return err
		}
	}
	return nil
}

// grant merges one default grant into a default-owned platform binding.
func (s *defaultSeeder) grant(ctx context.Context, block string, grant DefaultGrant) (string, string, error) {
	if _, ok := s.fileBlocks[block]; ok {
		return store.ExtensionDefaultSkipped, reasonFileBinding, nil
	}
	rows, err := s.st.ListBlockBindings(ctx, store.ScopePlatform, nil)
	if err != nil {
		return "", "", fmt.Errorf("app: load platform block bindings: %w", err)
	}
	var current *store.BlockBinding
	for i := range rows {
		if rows[i].Block == block {
			current = &rows[i]
			break
		}
	}
	snapshotKey := "block:" + block
	snapshot, owned := s.ledger[snapshotKey]
	switch {
	case current == nil && owned:
		return store.ExtensionDefaultSkipped, reasonCustomBinding, nil
	case current != nil && (!owned || !sameDefaultBinding(*current, snapshot.Detail)):
		return store.ExtensionDefaultSkipped, reasonCustomBinding, nil
	}
	merged := store.BlockBinding{Scope: store.ScopePlatform, Block: block}
	if current != nil {
		merged.MCPTools = append(merged.MCPTools, current.MCPTools...)
		merged.Skills = append(merged.Skills, current.Skills...)
	}
	merged.MCPTools = appendMissing(merged.MCPTools, grant.MCPTools)
	merged.Skills = appendMissing(merged.Skills, grant.Skills)
	if _, err := s.st.UpsertBlockBinding(ctx, merged); err != nil {
		return "", "", fmt.Errorf("app: write default block binding %q: %w", block, err)
	}
	detail, err := encodeDefaultBinding(merged)
	if err != nil {
		return "", "", err
	}
	if err := s.record(ctx, snapshotKey, store.ExtensionDefaultApplied, detail); err != nil {
		return "", "", err
	}
	return store.ExtensionDefaultApplied, "", nil
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
// the row behaves like an uploaded skill: it is listed, deletable and loaded
// from disk.
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
				rows, err := s.store.ListSkills(ctx, store.ScopePlatform, nil)
				if err != nil {
					return false, err
				}
				for _, row := range rows {
					if row.Name == name {
						return true, nil
					}
				}
				return false, nil
			},
			Install: func(ctx context.Context) error {
				_, err := s.installBuiltinSkill(ctx, name)
				return err
			},
			Bindings: bindings,
		})
	}
	return out, nil
}

// installBuiltinSkill stages the embedded files below the platform skills
// root and persists them through the same path as an upload.
func (s *ExtensionService) installBuiltinSkill(ctx context.Context, name string) (*Skill, error) {
	files, err := builtin.Sub(name)
	if err != nil {
		return nil, err
	}
	scopeRoot, err := s.skillScopeRoot(store.ScopePlatform, nil)
	if err != nil {
		return nil, err
	}
	staged, err := os.MkdirTemp(scopeRoot, ".builtin-*")
	if err != nil {
		return nil, fmt.Errorf("app: create skill directory: %w", err)
	}
	if err := copyEmbeddedSkill(files, staged); err != nil {
		_ = os.RemoveAll(staged)
		return nil, err
	}
	manifestName, description, err := readSkillManifest(staged)
	if err != nil || manifestName != name {
		_ = os.RemoveAll(staged)
		return nil, fmt.Errorf("app: bundled skill %q has an invalid manifest", name)
	}
	return s.persistSkillDir(ctx, store.ScopePlatform, nil, name, description, scopeRoot, staged)
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
func seedDefaultExtensions(ctx context.Context, cfg *config.Config, st *sqlite.Store, logger *slog.Logger, now func() time.Time) error {
	if len(cfg.DefaultExtensions.Skills) == 0 {
		return nil
	}
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:     st,
		Workspace: cfg.Workspace.Root,
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
	if err := ApplyExtensionDefaults(ctx, st, cfg.Blocks, defaults, logger); err != nil {
		return fmt.Errorf("app: apply default extensions: %w", err)
	}
	return nil
}
