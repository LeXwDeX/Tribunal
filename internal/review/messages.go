package review

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Review nodes and coverage checks store English machine messages in the
// workflow output; the console shows those originals for diagnosis. A published
// merge-request comment is user-facing text, so this file maps the known
// templates to Chinese at render time. Unknown machine text degrades to a
// generic Chinese sentence instead of leaking the original to the reader.

const (
	diagnosticFallback   = "上游评审通道未完成，具体原因与日志见控制台。"
	incompleteNextStep   = "下一步：在控制台打开本次评审详情，查看未完成通道的诊断与日志；修复后重新触发评审。"
	unknownWarningNotice = "评审通道出现异常，具体原因与日志见控制台。"
)

var (
	laneIncompleteRE    = regexp.MustCompile(`^lane \S+ \(([^)]+)\) did not complete: (\S+)(?: \([^)]*\))?$`)
	partialLaneRE       = regexp.MustCompile(`^lane \S+ \(([^)]+)\) stopped before submit_result: error_kind=(\S+) turns=(\d+) input_tokens=(\d+) output_tokens=(\d+) recovered_findings=(\d+)$`)
	nodeUnavailableRE   = regexp.MustCompile(`^node \S+ \(([^)]+)\) was (\S+) and its findings are unavailable$`)
	nodeNoExploreRE     = regexp.MustCompile(`^node \S+ \(([^)]+)\) was (\S+); the lanes ran without the exploration digest$`)
	coverageOmittedRE   = regexp.MustCompile(`^(?:diff collection omitted|omitted) (\d+) of (\d+) changed file\(s\)`)
	coveragePathsRE     = regexp.MustCompile(`; omitted paths: (.+)$`)
	coverageBeforePaths = regexp.MustCompile(`before review: \[([^\]]+)\]`)
	coverageReasonsRE   = regexp.MustCompile(`; reasons: ([^;]+)`)
	builtPromptGapRE    = regexp.MustCompile(`^built prompt did not include the complete diff for (\d+) of (\d+) file\(s\)`)
	planPromptLimitRE   = regexp.MustCompile(`^plan prompt lists at most (\d+) files; (\d+) changed files were omitted before planning$`)
	reviewTruncatedRE   = regexp.MustCompile(`^review truncated: (\d+) changed file\(s\) omitted by (max_files|max_diff_lines)$`)
	noReviewableCountRE = regexp.MustCompile(`^no reviewable files: (\d+) changed file\(s\) omitted before review$`)
	repoConfigFieldRE   = regexp.MustCompile(`^\.gitlab-review\.yml: unknown field ((?:review\.)?[A-Za-z0-9_.-]+) ignored$`)
	// Templates added with the review-scope completeness work (#455).
	withheldDiffRE          = regexp.MustCompile(`^review coverage gap: GitLab withheld the diff for (.+) \((too_large|collapsed)\); the file was not reviewed$`)
	generatedSkippedRE      = regexp.MustCompile(`^review skipped (\d+) generated file\(s\), which were not reviewed: (.+)$`)
	gitlabCollectionGapRE   = regexp.MustCompile(`^review coverage gap: GitLab diff collection is incomplete \(state=([^,]+), real_size=([^)]+)\): (\d+) of (\d+) changed file\(s\) were provided; the remaining files were not reviewed$`)
	gitlabTruncatedGapRE    = regexp.MustCompile(`^diff collection was truncated by the GitLab diff limits: GitLab reported (\d+) changed file\(s\); (\d+) were delivered \([^)]*\); (\d+) file\(s\) were not delivered and were not reviewed$`)
	gitlabNotCollectedGapRE = regexp.MustCompile(`^diff collection is marked incomplete by GitLab \(the diff version is not collected\): (\d+) changed file\(s\) were provided; whether every changed file was reviewed cannot be proven$`)
	generatedNoReviewRE     = regexp.MustCompile(`^no reviewable files: (\d+) generated file\(s\) were skipped$`)
	outOfScopeSevereRE      = regexp.MustCompile(`^incremental review ignored a (P0|P1) finding outside the reviewed file set: (.+)$`)
)

// kindLabels names review channels and coverage checks without exposing the
// internal node IDs that identify them in the workflow.
var kindLabels = map[string]string{
	BlockCodeReview:      "代码审查",
	BlockReviewStandards: "规范评审",
	BlockReviewIntent:    "意图评审",
	BlockReview:          "评审",
	BlockReviewGate:      "复核",
	BlockReasoner:        "推理评审",
	BlockValidate:        "验证评审",
	BlockExplore:         "探索",
	BlockPlan:            "评审计划",
	BlockSynthesize:      "汇总",
	"coverage":           "变更文件收集",
	"provenance":         "文件归属校验",
	"budget":             "评审预算",
}

var statusLabels = map[string]string{
	"skipped":   "已跳过",
	"failed":    "失败",
	"aborted":   "已取消",
	"cancelled": "已取消",
	"completed": "已完成",
	"pending":   "待执行",
	"running":   "进行中",
}

// failureClassLabels names the fixed run-failure classes that can appear on a
// degraded lane. Unknown values stay unnamed instead of leaking machine text.
var failureClassLabels = map[string]string{
	noSubmitClass:       "未提交最终结果",
	invalidSubmitClass:  "最终结果未通过校验",
	contextTooLongClass: "上下文超出模型窗口",
}

func failureClassLabel(class string) string {
	if label, ok := failureClassLabels[class]; ok {
		return label
	}
	return "固定失败类"
}

var coverageReasonLabels = map[string]string{
	"too_large":           "文件过大",
	"collapsed":           "diff 被折叠",
	"max_files":           "超过文件数量上限",
	"max_diff_lines":      "超过 diff 行数上限",
	"invalid_path":        "路径无效",
	"missing_rename_path": "重命名路径缺失",
}

// diffStateLabels names a GitLab diff version state. An unknown state stays
// unnamed rather than leaking machine text.
var diffStateLabels = map[string]string{
	"collected":     "已完整收集",
	"overflow":      "超出收集上限",
	"without_files": "未保存 diff",
	"unknown":       "状态未知",
}

// exactDiagnostics maps message templates without parameters.
var exactDiagnostics = map[string]string{
	"budget exhausted: some findings were not verified":                                             "预算耗尽：部分问题未完成二次验证，已按存疑处理。",
	"token budget exhausted before model synthesis; deterministic rules settlement used":            "预算耗尽：模型汇总未运行，已改用规则判定。",
	"token budget exhausted during model synthesis; deterministic rules settlement used":            "预算耗尽：模型汇总中断，已改用规则判定。",
	"model synthesis transport failed; deterministic rules settlement used":                         "模型汇总调用失败，已改用规则判定。",
	"workflow token budget exhausted before the remaining review scope was verified or synthesized": "预算耗尽：剩余评审范围未完成核验或汇总。",
	"unresolved evidence or review-level blocking cannot be settled as PASS":                        "存在未解决的证据不足或阻断，不能判定为通过。",
	"evidence is insufficient":                                                                        "评审通道报告证据不足。",
	"review channel reported BLOCKING with no findings":                                               "评审通道判定阻断，但未给出具体问题。",
	"review gate reported BLOCKING with no accepted findings":                                         "复核通道判定阻断，但未接受任何具体问题。",
	"finding path may refer to multiple or omitted changed files; file attribution is unproven":       "部分问题无法确定对应文件，文件归属未被证实。",
	"validate lane reported FAIL without a P0/P1 finding in the review scope":                         "验证通道判定失败，但评审范围内没有能解释该结论的 P0/P1 问题。",
	"ignore policy unavailable; rerun review":                                                         "忽略规则不可用，需要重新运行评审。",
	"workspace material identity unavailable; rerun review":                                           "工作区材料身份不可用，需要重新运行评审。",
	"repository skill provenance unavailable or unsupported; rerun review":                            "仓库 Skill 来源不可用或不受支持，需要重新运行评审。",
	"review execution failed":                                                                         "评审执行失败，详见控制台诊断。",
	"empty diff":                                                                                      "MR 的 diff 为空，无法评审。",
	"no reviewable files after ignore rules":                                                          "忽略规则过滤后没有可评审的文件。",
	"no changed files in merge request":                                                               "MR 中没有变更文件。",
	"incremental review: findings outside the incremental file set were ignored":                      "增量评审忽略了本次范围外的问题。",
	"new findings await verification and were not published":                                          "汇总阶段新增的问题未经核验，未发布。",
	"max_nodes limit reached: extension requests were not scheduled":                                  "达到节点上限，部分扩展评审未排期。",
	"extension review findings are marked uncertain":                                                  "扩展评审发现已按存疑发布。",
	"package name was truncated before review":                                                        "部分评审包名称过长，已截断。",
	"package focus was truncated before review":                                                       "部分评审包关注点过长，已截断。",
	backgroundRulesTruncated:                                                                          "项目背景超出长度上限，部分项目规则未被使用，评审结论可能受影响。",
	backgroundContextTruncated:                                                                        "项目背景超出长度上限，部分项目上下文文档被省略。",
	"review ran without the code graph: project ignore rules blocked workspace indexing":              "本次评审未使用代码图谱：项目忽略规则阻止了工作区索引。",
	"review ran without the project context document: the stored document is not verified for review": "本次评审未使用项目上下文文档：已存储的文档未通过评审校验。",
	"review coverage gap: the merge request base commit is unavailable; changed lines were reviewed without a base checkout": "评审范围缺口：未能取得 MR 的 base 提交，变更行在缺少 base 检出（base checkout）的情况下评审。",
}

// localizeMachineMessage translates one known machine message. The second
// result reports whether a template matched; callers decide the fallback.
func localizeMachineMessage(message string) (string, bool) {
	message = strings.TrimSpace(message)
	if message == "" {
		return "", false
	}
	if text, ok := exactDiagnostics[message]; ok {
		return text, true
	}
	if match := partialLaneRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("%s通道在提交最终结果前结束（%s，%s 轮，输入 %s / 输出 %s tokens），已保留恢复的 %s 条问题；该通道未完成，结论不能为通过。",
			kindLabel(match[1]), failureClassLabel(match[2]), match[3], match[4], match[5], match[6]), true
	}
	if match := laneIncompleteRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("%s通道未完成（%s）%s",
			kindLabel(match[1]), statusLabel(match[2]), laneAdvice(match[2])), true
	}
	if match := nodeUnavailableRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("%s通道未完成（%s），该通道的问题发现不可用%s",
			kindLabel(match[1]), statusLabel(match[2]), laneAdvice(match[2])), true
	}
	if match := nodeNoExploreRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("探索通道未完成（%s），各评审通道在没有探索摘要的情况下运行，结论可能不完整。",
			statusLabel(match[2])), true
	}
	if match := withheldDiffRE.FindStringSubmatch(message); match != nil {
		reason := coverageReasonLabels[match[2]]
		if reason == "" {
			reason = "diff 不可用"
		}
		return fmt.Sprintf("GitLab 未提供 %s 的 diff（%s），该文件未被评审。", match[1], reason), true
	}
	if match := generatedSkippedRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("已跳过 %s 个生成文件，未进入评审：%s。", match[1], match[2]), true
	}
	if match := gitlabCollectionGapRE.FindStringSubmatch(message); match != nil {
		provided, total := atoiOrZero(match[3]), atoiOrZero(match[4])
		remaining := total - provided
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Sprintf("GitLab 的变更文件收集不完整（状态：%s，报告 %d 个变更文件）：仅收到 %d 个文件，其余 %d 个文件未被评审。",
			diffStateLabel(match[1]), total, provided, remaining), true
	}
	if match := gitlabTruncatedGapRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("GitLab 的变更文件收集被截断：报告 %s 个变更文件，实际收到 %s 个，有 %s 个文件未送达、未被评审。",
			match[1], match[2], match[3]), true
	}
	if match := gitlabNotCollectedGapRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("GitLab 未完整收集该 MR 的 diff：已收到 %s 个变更文件，无法证明所有变更文件都已评审。",
			match[1]), true
	}
	if match := generatedNoReviewRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("没有可评审的文件：已跳过 %s 个生成文件。", match[1]), true
	}
	if match := outOfScopeSevereRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("增量评审未重新检查范围外文件 %s 中的 %s 问题，该问题已被过滤，结论不覆盖该文件。",
			match[2], match[1]), true
	}
	if match := coverageOmittedRE.FindStringSubmatch(message); match != nil {
		text := fmt.Sprintf("变更文件收集不完整：共 %s 个变更文件，有 %s 个未进入评审材料%s",
			match[2], match[1], coverageReasonText(message))
		text = strings.TrimSuffix(text, "。")
		if paths := coverageOmittedPaths(message); paths != "" {
			text += "；未进入评审的路径：" + paths
		}
		return text + "。", true
	}
	if match := builtPromptGapRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("有 %s 个文件的完整 diff 未进入评审提示，相关内容可能未被完整评审。", match[1]), true
	}
	if match := planPromptLimitRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("评审计划阶段遗漏了 %s 个变更文件（提示词最多列出 %s 个）。", match[2], match[1]), true
	}
	if match := reviewTruncatedRE.FindStringSubmatch(message); match != nil {
		limit := "文件数量上限"
		if match[2] == "max_diff_lines" {
			limit = "diff 行数上限"
		}
		return fmt.Sprintf("受%s限制，有 %s 个变更文件未纳入评审。", limit, match[1]), true
	}
	if match := noReviewableCountRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("没有可评审的文件：有 %s 个变更文件在评审前被遗漏。", match[1]), true
	}
	if match := repoConfigFieldRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("仓库配置 .gitlab-review.yml 中的未知字段 %s 已忽略。", match[1]), true
	}
	if message == ".gitlab-review.yml: language is too long and was ignored" {
		return "仓库配置 .gitlab-review.yml 中的语言过长，已被忽略。", true
	}
	switch {
	case strings.HasPrefix(message, "diff collection is truncated:"):
		return "变更文件收集被截断，旧快照没有记录被省略的路径，未评审范围无法完整确定。", true
	case strings.HasPrefix(message, "review coverage gap: skipped a changed file with an invalid path"):
		return "有变更文件的路径无效，未能评审。", true
	case strings.HasPrefix(message, "review coverage gap: rename paths are incomplete for"):
		return "重命名文件的路径信息不完整，相关内容可能未被完整评审。", true
	case strings.HasPrefix(message, "review coverage gap: GitLab did not provide a diff for"):
		return "GitLab 未提供部分文件的 diff，相关内容未被评审。", true
	case strings.HasPrefix(message, "no textual changes to review:"):
		return "部分文件没有文本变更可评审。", true
	case strings.HasPrefix(message, "file ") && strings.HasSuffix(message, "is not in the materials snapshot; it was not reviewed"):
		return "有文件未进入评审材料，未被评审。", true
	case strings.HasPrefix(message, "file ") && strings.HasSuffix(message, "has no diff content in the materials snapshot; it was not reviewed"):
		return "有文件在评审材料中没有 diff 内容，未被评审。", true
	case strings.HasPrefix(message, "planner assigned ") && strings.Contains(message, "reviewable included file(s) to review nodes"):
		return "评审计划未能把所有可评审文件分配到评审通道，部分文件未被评审。", true
	case strings.HasPrefix(message, "cross challenge "):
		return "复核通道无法使用独立模型完成交叉验证，相关问题已按存疑处理。", true
	case (strings.HasPrefix(message, "unresolved judge group ") || strings.HasPrefix(message, "group ")) &&
		strings.HasSuffix(message, "was marked uncertain"):
		return "部分问题无法完成裁决，已按存疑处理。", true
	case strings.HasPrefix(message, "new finding ") &&
		(strings.HasSuffix(message, "has no completed verification") || strings.HasSuffix(message, "has no verification result")):
		return "部分新增问题未完成二次验证，未发布。", true
	case strings.HasPrefix(message, "verify failed for group "), strings.HasPrefix(message, "verify result missing for group "):
		return "部分问题的二次验证未完成，已按存疑处理。", true
	case strings.Contains(message, "MCP tools unavailable"):
		return "部分评审工具不可用，相关范围可能未被完整评审。", true
	}
	return "", false
}

func kindLabel(kind string) string {
	if label := kindLabels[kind]; label != "" {
		return label
	}
	return "上游"
}

func atoiOrZero(value string) int {
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 {
		return 0
	}
	return count
}

func diffStateLabel(state string) string {
	if label := diffStateLabels[state]; label != "" {
		return label
	}
	return diffStateLabels["unknown"]
}

func statusLabel(status string) string {
	if label := statusLabels[status]; label != "" {
		return label
	}
	return "状态未知"
}

func laneAdvice(status string) string {
	switch status {
	case "failed", "aborted", "cancelled":
		return "：该通道执行失败，建议查看控制台中的失败原因后重新触发评审。"
	case "skipped":
		return "：该通道未运行，建议检查评审配置后重新触发评审。"
	default:
		return "：建议在控制台查看该通道状态后重新触发评审。"
	}
}

// coverageOmittedPaths returns the recorded omitted paths of a coverage
// message. Both the workflow signal and the standalone summary reason carry
// them; an older snapshot without paths returns nothing.
func coverageOmittedPaths(message string) string {
	if match := coveragePathsRE.FindStringSubmatch(message); match != nil {
		return strings.TrimSpace(match[1])
	}
	if match := coverageBeforePaths.FindStringSubmatch(message); match != nil {
		return strings.TrimSpace(match[1])
	}
	return ""
}

// coverageReasonText renders the machine reason names of a coverage message.
// An unknown reason stays unnamed rather than leaking its machine token.
func coverageReasonText(message string) string {
	match := coverageReasonsRE.FindStringSubmatch(message)
	if match == nil {
		return "。"
	}
	parts := make([]string, 0, 4)
	for _, item := range strings.Split(match[1], ", ") {
		name, count, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			continue
		}
		label := coverageReasonLabels[name]
		if label == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s 个", label, count))
	}
	if len(parts) == 0 {
		return "，原因详见控制台。"
	}
	return "（原因：" + strings.Join(parts, "、") + "）。"
}

// localizeWarning renders one 评审提示 line. Chinese warning text is already
// user-facing and kept as is; unmatched English prose falls back to a generic
// Chinese sentence, while short literal tokens (for example a GitLab reference)
// keep their literal rendering.
func localizeWarning(warning string) string {
	warning = strings.TrimSpace(warning)
	if warning == "" {
		return ""
	}
	if text, ok := localizeMachineMessage(warning); ok {
		return text
	}
	if hasHan(warning) || !looksMachineEnglish(warning) {
		return warning
	}
	return unknownWarningNotice
}

// localizeIncompleteReason renders the reason line of an incomplete summary.
func localizeIncompleteReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	if text, ok := localizeMachineMessage(reason); ok {
		return text
	}
	if hasHan(reason) {
		return reason
	}
	return "具体原因与日志见控制台。"
}

func hasHan(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// looksMachineEnglish reports whether unmatched text reads as English prose
// rather than a short literal token such as GL-71.
func looksMachineEnglish(value string) bool {
	words := 0
	for _, field := range strings.FieldsFunc(value, func(r rune) bool {
		return !(r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.'))
	}) {
		if len(field) >= 3 {
			words++
		}
	}
	return words >= 2
}

// localizeIncompleteSummary rewrites the deterministic diagnostic summary into
// 「已评审范围结论 + 中文未完成原因 + 下一步」. The original signal text stays in
// the stored workflow and the console. It reports false for any other summary,
// which is rendered unchanged.
func localizeIncompleteSummary(summary string, inline, summaryOnly int) (string, bool) {
	trimmed := strings.TrimSpace(summary)
	if !strings.HasPrefix(trimmed, unresolvedSummaryPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(trimmed, unresolvedSummaryPrefix)
	extra := 0
	if index := strings.LastIndex(rest, "；另有 "); index >= 0 {
		if count, err := strconv.Atoi(strings.TrimSuffix(rest[index+len("；另有 "):], " 项")); err == nil {
			extra = count
		}
		rest = rest[:index]
	}
	reasons := make([]string, 0, 8)
	seen := make(map[string]bool, 8)
	for _, item := range strings.Split(rest, "；") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		node, message := "", item
		if index := strings.Index(item, ": "); index >= 0 {
			node, message = item[:index], item[index+len(": "):]
		}
		text := localizeUnresolvedItem(node, message)
		if !seen[text] {
			seen[text] = true
			reasons = append(reasons, text)
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, diagnosticFallback)
	}
	if extra > 0 {
		reasons = append(reasons, fmt.Sprintf("另有 %d 项检查未完成，详见控制台。", extra))
	}
	var b strings.Builder
	b.WriteString("**已评审范围的结论**：")
	if inline+summaryOnly == 0 {
		b.WriteString("已完成部分没有可发布的问题发现；未完成通道覆盖的代码不在结论内，不能据此判定通过。")
	} else {
		fmt.Fprintf(&b, "已完成部分共发布 %d 条问题，见下方列表；未完成通道覆盖的代码不在结论内。", inline+summaryOnly)
	}
	b.WriteString("\n\n**未完成原因**\n\n")
	for _, reason := range reasons {
		b.WriteString("- ")
		b.WriteString(reason)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(incompleteNextStep)
	return b.String(), true
}

// localizeUnresolvedItem translates one parsed unresolved signal.
func localizeUnresolvedItem(node, message string) string {
	if text, ok := localizeMachineMessage(message); ok {
		return text
	}
	if label := kindLabels[node]; label != "" {
		return label + "未完成，具体原因与日志见控制台。"
	}
	if hasHan(message) {
		return message
	}
	return diagnosticFallback
}
