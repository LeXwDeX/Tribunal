package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type reviewExecution struct {
	blocks            map[string]config.BlockBinding
	tools             review.ToolProvider
	skills            review.SkillProvider
	digest            []byte
	skillRoots        map[string]string
	repository        *review.RepositorySkillProvenance
	repositoryCleanup func()
}

// resolveReviewExecution requires the project workspace lock. It pins the
// resolver set for identity and execution without contacting an MCP server.
func (s *ReviewService) resolveReviewExecution(ctx context.Context, project *store.Project, token string, profile review.Profile, targetBranch string, deny func(string) bool) (*reviewExecution, error) {
	activeBlocks, err := reviewExecutionSkillBlocks(profile, s.allowedProviders())
	if err != nil {
		return nil, PermanentJob(fmt.Errorf("build review graph: %w", err))
	}
	execution := &reviewExecution{blocks: s.blocks, tools: s.tools, skills: s.skills, skillRoots: make(map[string]string)}
	var mcpConfig *mcp.Config
	var deniedEnv []string
	if s.extensions != nil {
		set, err := s.extensions.ForProject(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		if set == nil {
			return nil, errors.New("app: project execution identity unavailable")
		}
		execution.blocks, execution.tools, execution.skills = set.blocks, set.tools, set.skills
		mcpConfig = &set.mcpConfig
		if len(set.mcpConfig.MCPServers) > 0 {
			deniedEnv = append([]string(nil), s.extensions.deniedEnv...)
		}
	}
	repository, err := s.resolveRepositorySkillRuntime(ctx, project, token, targetBranch, deny, execution.blocks, execution.skills)
	if err != nil {
		return nil, err
	}
	execution.blocks, execution.skills = repository.blocks, repository.skills
	execution.repository, execution.repositoryCleanup = repository.provenance, repository.cleanup
	resolved := false
	defer func() {
		if !resolved {
			execution.cleanupRepositorySkills()
		}
	}()
	var skillIdentity = make(map[string]any)
	if s.extensions != nil || execution.repositorySkillsVerified() {
		for block, binding := range execution.blocks {
			if !activeBlocks[block] || len(binding.Skills) == 0 {
				continue
			}
			if execution.skills == nil {
				return nil, errors.New("app: bound skill identity unavailable")
			}
			bound, err := execution.skills.BindSkills(ctx, binding.Skills)
			if err != nil {
				return nil, fmt.Errorf("app: bound skill identity unavailable: %w", err)
			}
			if (bound.Tool != nil || len(bound.Skills) > 0) && len(bound.Roots) != len(bound.Skills) {
				return nil, errors.New("app: bound skill identity unavailable")
			}
			roots := append([]string(nil), bound.Roots...)
			sort.Strings(roots)
			files := make([]string, 0, len(roots))
			for _, root := range roots {
				identity, err := reviewSkillTreeHash(ctx, root)
				if err != nil {
					return nil, err
				}
				if previous, ok := execution.skillRoots[root]; ok && previous != identity {
					return nil, errors.New("app: skill identity changed while resolving")
				}
				execution.skillRoots[root] = identity
				files = append(files, identity)
			}
			skillIdentity[block] = struct {
				Skills []review.Skill
				Trees  []string
			}{bound.Skills, files}
		}
	}
	var repositoryIdentity json.RawMessage
	if execution.repositorySkillsVerified() {
		repositoryIdentity, err = json.Marshal(execution.repository)
		if err != nil {
			return nil, errors.New("app: execution identity unavailable")
		}
	}
	// Repository is appended last and omitted for none so an existing workflow
	// with explicit none keeps its stored execution identity.
	data, err := json.Marshal(struct {
		Providers  json.RawMessage
		Blocks     map[string]config.BlockBinding
		MCP        *mcp.Config
		DeniedEnv  []string
		Skills     map[string]any
		Synthetic  string
		Repository json.RawMessage `json:",omitempty"`
	}{s.providerConfig, execution.blocks, mcpConfig, deniedEnv, skillIdentity, s.testIdentity, repositoryIdentity})
	if err != nil {
		return nil, errors.New("app: execution identity unavailable")
	}
	sum := sha256.Sum256(data)
	execution.digest = sum[:]
	resolved = true
	return execution, nil
}

// Initial graph nodes and their expanders determine which bindings can reach
// callAgent. Reasoner and synthesis currently pass no block to callAgent.
func reviewExecutionSkillBlocks(profile review.Profile, providers []string) (map[string]bool, error) {
	spec, err := review.BuildGraph(profile, &review.Dossier{}, review.GraphConfig{AllowedProviders: providers})
	if err != nil {
		return nil, err
	}
	blocks := make(map[string]bool)
	for _, node := range spec.Nodes {
		switch node.Kind {
		case review.KindOnboarding:
			blocks[review.BlockOnboarding] = true
		case review.KindCodeReview:
			blocks[review.BlockCodeReview] = true
		case review.KindReviewStd, review.KindReviewInt:
			blocks["review"] = true
		case review.KindVerify:
			blocks[review.BlockVerify] = true
		case review.KindPlan:
			if node.Expands {
				var params struct {
					Propose []struct {
						Block string `json:"block"`
					} `json:"propose"`
				}
				if err := json.Unmarshal(node.Params, &params); err != nil {
					return nil, err
				}
				for _, propose := range params.Propose {
					switch propose.Block {
					case review.BlockCodeReview, "review":
						blocks[propose.Block] = true
					default:
						return nil, errors.New("app: unknown review graph binding")
					}
				}
			}
		case review.KindGather:
			if node.Expands {
				blocks[review.BlockVerify] = true
			}
		case review.KindSynthesize:
			if node.Expands {
				blocks[review.BlockCodeReview] = true
				blocks[review.BlockVerify] = true
			}
		case review.KindReasoner, review.KindReviewGate, review.KindAdjudicate:
		default:
			return nil, errors.New("app: unknown review graph node")
		}
	}
	return blocks, nil
}

func (e *reviewExecution) agentRunner(runner review.AgentRunner) review.AgentRunner {
	if len(e.skillRoots) == 0 {
		return runner
	}
	if runner == nil {
		runner = agent.Run
	}
	check := func(ctx context.Context) error {
		for root, expected := range e.skillRoots {
			actual, err := reviewSkillTreeHash(ctx, root)
			if err != nil || actual != expected {
				return errors.New("app: bound skill changed during review")
			}
		}
		return nil
	}
	return func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		if err := check(ctx); err != nil {
			return nil, err
		}
		result, err := runner(ctx, spec)
		if ctx.Err() != nil {
			return result, err
		}
		if checkErr := check(ctx); checkErr != nil {
			return result, checkErr
		}
		return result, err
	}
}

func reviewSkillTreeHash(ctx context.Context, dir string) (string, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("app: skill identity unavailable")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", errors.New("app: skill identity unavailable")
	}
	defer root.Close()
	hash := sha256.New()
	var count int
	var total int64
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unsafe skill identity")
		}
		if entry.IsDir() {
			if strings.EqualFold(entry.Name(), ".git") {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("unsafe skill identity")
		}
		count++
		total += info.Size()
		if count > 4096 || total > 64<<20 {
			return errors.New("skill identity exceeds limit")
		}
		file, err := root.Open(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%d:%s:%d:", len(path), path, info.Size())
		copied, copyErr := io.Copy(hash, io.LimitReader(file, info.Size()+1))
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if copied != info.Size() {
			return errors.New("skill changed while reading")
		}
		return closeErr
	})
	if err != nil {
		return "", errors.New("app: skill identity unavailable")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func reviewExecutionAAD(projectID int64) []byte {
	return []byte(fmt.Sprintf("tribunal/review-execution/v1/%d", projectID))
}

func (s *ReviewService) sealReviewExecution(projectID int64, digest []byte) (string, error) {
	sealed, err := s.cipher.Seal(digest, reviewExecutionAAD(projectID))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *ReviewService) matchesReviewExecution(projectID int64, encoded string, digest []byte) bool {
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	stored, err := s.cipher.Open(sealed, reviewExecutionAAD(projectID))
	return err == nil && len(stored) == sha256.Size && string(stored) == string(digest)
}
