package taskpilot

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

var snapshotMarkdownSections = []struct {
	Title string
	Key   string
}{
	{"Recent Changes", "recent_changes"},
	{"Key Decisions", "key_decisions"},
	{"Important Reasoning", "reasoning"},
	{"Open Questions", "open_questions"},
	{"Implementation Direction", "implementation_direction"},
	{"Files / Components", "files_or_components"},
	{"Risks", "risks"},
	{"Blockers", "blockers"},
	{"Assumptions", "assumptions"},
	{"Next Recommended Actions", "next_recommended_actions"},
}

var handoffMarkdownSections = []struct {
	Title string
	Key   string
}{
	{"Objective", "task_objective"},
	{"Original Requirements", "original_requirements"},
	{"Current Status", "current_status"},
	{"Current State", "current_state"},
	{"Handoff Timeline", "handoff_timeline"},
	{"Completed Work", "completed_work"},
	{"Important Decisions", "important_decisions"},
	{"Rejected Approaches", "rejected_approaches"},
	{"Architecture Notes", "architecture_notes"},
	{"Implementation Notes", "implementation_notes"},
	{"Files / Components Affected", "files_components_affected"},
	{"Known Issues", "known_issues"},
	{"Failed / Interrupted Sessions", "failed_sessions"},
	{"Remaining Work", "remaining_work"},
	{"Suggested Next Steps", "suggested_next_steps"},
	{"Assumptions", "assumptions"},
	{"Risks", "risks"},
	{"Dependencies", "dependencies"},
	{"Handoff Message", "handoff_message"},
}

func buildSnapshotContent(detail TaskDetail, entries []ContextEntry) (SnapshotContent, []string) {
	out := SnapshotContent{}
	sourceIDs := []string{}
	for _, entry := range entries {
		if isNoisyContext(entry.Content) {
			continue
		}
		sourceIDs = append(sourceIDs, entry.ID)
		content := strings.TrimSpace(entry.Content)
		switch entry.Kind {
		case "summary":
			out.RecentChanges = append(out.RecentChanges, content)
		case "decision":
			out.KeyDecisions = append(out.KeyDecisions, content)
		case "risk":
			out.Risks = append(out.Risks, content)
		case "blocker":
			out.Blockers = append(out.Blockers, content)
		case "output_ref":
			out.FilesOrComponents = appendUseful(out.FilesOrComponents, conciseOutputRef(content))
		case "note":
			out.Reasoning = append(out.Reasoning, content)
		case "next":
			out.NextRecommendedActions = append(out.NextRecommendedActions, content)
		}
	}
	for _, decision := range detail.Decisions {
		out.KeyDecisions = append(out.KeyDecisions, formatDecision(decision))
	}
	out.Risks = append(out.Risks, detail.Task.Risks...)
	out.Blockers = append(out.Blockers, detail.Task.Blockers...)
	for _, gitRef := range detail.GitRefs {
		if gitRef.Branch != "" {
			out.FilesOrComponents = append(out.FilesOrComponents, "Branch: "+gitRef.Branch)
		}
		if gitRef.PRURL != "" {
			out.FilesOrComponents = append(out.FilesOrComponents, "PR: "+gitRef.PRURL)
		}
		out.FilesOrComponents = append(out.FilesOrComponents, gitRef.ChangedFiles...)
	}
	for _, artifact := range detail.Artifacts {
		out.FilesOrComponents = append(out.FilesOrComponents, fmt.Sprintf("%s: %s (%s)", artifact.Kind, artifact.Title, artifact.URI))
	}
	for _, handoff := range detail.Handoffs {
		if handoff.ResumeSummary != "" {
			out.Reasoning = append(out.Reasoning, "Handoff prepared: "+handoff.ResumeSummary)
		}
	}
	if out.ImplementationDirection == "" {
		out.ImplementationDirection = inferImplementationDirection(detail, out)
	}
	out.RecentChanges = limitStrings(uniqueStrings(out.RecentChanges), 12)
	out.KeyDecisions = limitStrings(uniqueStrings(out.KeyDecisions), 12)
	out.Reasoning = limitStrings(uniqueStrings(out.Reasoning), 10)
	out.OpenQuestions = limitStrings(uniqueStrings(out.OpenQuestions), 10)
	out.FilesOrComponents = limitStrings(uniqueStrings(out.FilesOrComponents), 16)
	out.Risks = limitStrings(uniqueStrings(out.Risks), 12)
	out.Blockers = limitStrings(uniqueStrings(out.Blockers), 12)
	out.Assumptions = limitStrings(uniqueStrings(out.Assumptions), 10)
	out.NextRecommendedActions = limitStrings(uniqueStrings(out.NextRecommendedActions), 12)
	return out, uniqueStrings(sourceIDs)
}

func buildHandoffPacketContent(detail TaskDetail, snapshots []ContextSnapshot) (HandoffPacketContent, []string, []string) {
	out := HandoffPacketContent{
		TaskObjective:        strings.TrimSpace(detail.Task.Goal),
		OriginalRequirements: append([]string{}, detail.Task.Requirements...),
		CurrentStatus:        detail.Task.Status,
		CurrentState:         inferHandoffCurrentState(detail),
	}
	sourceSnapshotIDs := []string{}
	sourceContextIDs := []string{}
	for _, snapshot := range snapshots {
		sourceSnapshotIDs = append(sourceSnapshotIDs, snapshot.ID)
		sourceContextIDs = append(sourceContextIDs, snapshot.SourceContextIDs...)
		out.CompletedWork = appendUseful(out.CompletedWork, snapshot.Summary.RecentChanges...)
		out.ImportantDecisions = appendUseful(out.ImportantDecisions, snapshot.Summary.KeyDecisions...)
		out.ImplementationNotes = appendUseful(out.ImplementationNotes, snapshot.Summary.Reasoning...)
		if snapshot.Summary.ImplementationDirection != "" && !isNoisyContext(snapshot.Summary.ImplementationDirection) {
			out.ImplementationNotes = append(out.ImplementationNotes, "Direction: "+snapshot.Summary.ImplementationDirection)
		}
		for _, item := range snapshot.Summary.FilesOrComponents {
			out.FilesComponentsAffected = appendUseful(out.FilesComponentsAffected, conciseOutputRef(item))
		}
		for _, blocker := range snapshot.Summary.Blockers {
			if isFailedRunContext(blocker) {
				out.FailedSessions = appendUseful(out.FailedSessions, normalizeFailedRunContext(blocker))
			} else {
				out.KnownIssues = appendUseful(out.KnownIssues, blocker)
			}
		}
		out.Risks = appendUseful(out.Risks, snapshot.Summary.Risks...)
		out.Assumptions = appendUseful(out.Assumptions, snapshot.Summary.Assumptions...)
	}
	latestHandoff := latestHandoffForTimeline(detail.Handoffs)
	nextFromContext := []string{}
	for _, entry := range detail.Context {
		if isNoisyContext(entry.Content) {
			continue
		}
		sourceContextIDs = append(sourceContextIDs, entry.ID)
		switch entry.Kind {
		case "summary":
			out.CompletedWork = appendUseful(out.CompletedWork, entry.Content)
		case "decision":
			out.ImportantDecisions = appendUseful(out.ImportantDecisions, entry.Content)
		case "risk":
			out.Risks = appendUseful(out.Risks, entry.Content)
		case "blocker":
			if isFailedRunContext(entry.Content) {
				out.FailedSessions = appendUseful(out.FailedSessions, normalizeFailedRunContext(entry.Content))
			} else {
				out.KnownIssues = appendUseful(out.KnownIssues, entry.Content)
			}
		case "output_ref":
			out.FilesComponentsAffected = appendUseful(out.FilesComponentsAffected, conciseOutputRef(entry.Content))
		case "note":
			out.ImplementationNotes = appendUsefulImplementationNotes(out.ImplementationNotes, entry.Content)
		case "next":
			if latestHandoff == nil || entry.CreatedAt.After(latestHandoff.CreatedAt) {
				nextFromContext = appendUseful(nextFromContext, entry.Content)
			}
		}
	}
	for _, decision := range detail.Decisions {
		out.ImportantDecisions = append(out.ImportantDecisions, formatDecision(decision))
	}
	for _, dep := range detail.Dependencies {
		label := dep.DependsOnID
		if dep.DependsOnTask != nil {
			label = fmt.Sprintf("%s (%s)", dep.DependsOnTask.Title, dep.DependsOnTask.Status)
		}
		out.Dependencies = append(out.Dependencies, label)
	}
	for _, gitRef := range detail.GitRefs {
		if gitRef.Branch != "" {
			out.FilesComponentsAffected = append(out.FilesComponentsAffected, "Branch: "+gitRef.Branch)
		}
		if gitRef.PRURL != "" {
			out.FilesComponentsAffected = append(out.FilesComponentsAffected, "PR: "+gitRef.PRURL)
		}
		out.FilesComponentsAffected = append(out.FilesComponentsAffected, gitRef.ChangedFiles...)
	}
	for _, artifact := range detail.Artifacts {
		out.FilesComponentsAffected = append(out.FilesComponentsAffected, fmt.Sprintf("%s: %s (%s)", artifact.Kind, artifact.Title, artifact.URI))
	}
	for _, handoff := range detail.Handoffs {
		if isUsefulHandoffMessage(handoff.ResumeSummary, detail.Task.Goal) {
			out.HandoffMessage = handoff.ResumeSummary
		}
	}
	out.HandoffTimeline = buildHandoffTimeline(detail)
	if latestHandoff != nil {
		out.SuggestedNextSteps = appendUseful(out.SuggestedNextSteps, latestHandoff.NextSteps...)
	}
	out.SuggestedNextSteps = appendUseful(out.SuggestedNextSteps, nextFromContext...)
	out.Risks = appendUseful(out.Risks, detail.Task.Risks...)
	for _, blocker := range detail.Task.Blockers {
		if isFailedRunContext(blocker) {
			out.FailedSessions = appendUseful(out.FailedSessions, normalizeFailedRunContext(blocker))
		} else {
			out.KnownIssues = appendUseful(out.KnownIssues, blocker)
		}
	}
	if len(out.SuggestedNextSteps) == 0 && detail.Task.Status != "completed" {
		out.SuggestedNextSteps = append(out.SuggestedNextSteps, "Continue from the latest task context and verify completion criteria.")
	}
	if len(out.CompletedWork) > 0 && !hasDecisionState(out.ImportantDecisions) {
		out.ImportantDecisions = append(out.ImportantDecisions, "No material decision made; work followed existing requirements.")
	}
	if len(out.RemainingWork) == 0 && len(out.CompletedWork) > 0 {
		if detail.Task.Status == "handoff_ready" {
			out.RemainingWork = append(out.RemainingWork, "No known document work remains; next agent should verify the handoff context and continue only if new requirements are added.")
		} else {
			out.RemainingWork = append(out.RemainingWork, "Verify the recorded work and decide whether to continue, send to review, publish a handoff, or mark complete.")
		}
	}
	if strings.TrimSpace(out.HandoffMessage) == "" && len(out.CompletedWork) > 0 {
		target := "the task"
		if len(out.FilesComponentsAffected) > 0 {
			target = strings.Join(limitStrings(uniqueStrings(out.FilesComponentsAffected), 3), ", ")
		}
		out.HandoffMessage = fmt.Sprintf("Continue from the recorded work on %s. Review completed work, known issues, and suggested next steps before making changes.", target)
	}
	out.CompletedWork = limitStrings(uniqueStrings(out.CompletedWork), 20)
	out.ImportantDecisions = limitStrings(uniqueStrings(out.ImportantDecisions), 20)
	out.HandoffTimeline = limitStrings(uniqueStrings(out.HandoffTimeline), 20)
	out.RejectedApproaches = limitStrings(uniqueStrings(out.RejectedApproaches), 12)
	out.ArchitectureNotes = limitStrings(uniqueStrings(out.ArchitectureNotes), 16)
	out.ImplementationNotes = limitStrings(uniqueStrings(out.ImplementationNotes), 20)
	out.FilesComponentsAffected = limitStrings(uniqueStrings(out.FilesComponentsAffected), 24)
	out.KnownIssues = limitStrings(uniqueStrings(out.KnownIssues), 16)
	out.FailedSessions = limitStrings(uniqueStrings(out.FailedSessions), 8)
	out.RemainingWork = limitStrings(uniqueStrings(out.RemainingWork), 16)
	out.SuggestedNextSteps = limitStrings(latestActionableNextSteps(out.SuggestedNextSteps), 8)
	out.Assumptions = limitStrings(uniqueStrings(out.Assumptions), 12)
	out.Risks = limitStrings(uniqueStrings(out.Risks), 16)
	out.Dependencies = limitStrings(uniqueStrings(out.Dependencies), 12)
	return out, uniqueStrings(sourceSnapshotIDs), uniqueStrings(sourceContextIDs)
}

func buildHandoffFallbackContent(detail TaskDetail) HandoffPacketContent {
	return buildHandoffPacketContentFromDetail(detail)
}

func buildHandoffPacketContentFromDetail(detail TaskDetail) HandoffPacketContent {
	packet, _, _ := buildHandoffPacketContent(detail, detail.Snapshots)
	return packet
}

func mergeAgentAuthoredHandoffWithFallback(agent HandoffPacketContent, detail TaskDetail) HandoffPacketContent {
	fallback := buildHandoffPacketContentFromDetail(detail)
	out := agent
	if strings.TrimSpace(out.TaskObjective) == "" {
		out.TaskObjective = fallback.TaskObjective
	}
	if len(out.OriginalRequirements) == 0 {
		out.OriginalRequirements = fallback.OriginalRequirements
	}
	if strings.TrimSpace(out.CurrentStatus) == "" {
		out.CurrentStatus = fallback.CurrentStatus
	}
	if strings.TrimSpace(out.CurrentState) == "" || isHandoffPlaceholder(out.CurrentState) || strings.HasPrefix(strings.TrimSpace(out.CurrentState), "Task is ") {
		out.CurrentState = fallback.CurrentState
	}
	if len(out.HandoffTimeline) == 0 {
		out.HandoffTimeline = fallback.HandoffTimeline
	}
	if listMissingOrPlaceholder(out.CompletedWork) {
		out.CompletedWork = fallback.CompletedWork
		if len(out.CompletedWork) == 0 && len(fallback.FilesComponentsAffected) > 0 {
			out.CompletedWork = append(out.CompletedWork, "Updated task files: "+strings.Join(fallback.FilesComponentsAffected, ", "))
		}
	}
	if !hasDecisionState(out.ImportantDecisions) {
		out.ImportantDecisions = fallback.ImportantDecisions
	}
	if len(out.RejectedApproaches) == 0 {
		out.RejectedApproaches = fallback.RejectedApproaches
	}
	if len(out.ArchitectureNotes) == 0 {
		out.ArchitectureNotes = fallback.ArchitectureNotes
	}
	out.ImplementationNotes = appendUsefulImplementationNotes(nil, out.ImplementationNotes...)
	if len(out.FilesComponentsAffected) == 0 {
		out.FilesComponentsAffected = fallback.FilesComponentsAffected
	}
	if len(out.KnownIssues) == 0 {
		out.KnownIssues = fallback.KnownIssues
	}
	if len(out.FailedSessions) == 0 {
		out.FailedSessions = fallback.FailedSessions
	}
	if listMissingOrPlaceholder(out.RemainingWork) {
		out.RemainingWork = fallback.RemainingWork
		if len(out.RemainingWork) == 0 && detail.Task.Status != "completed" {
			out.RemainingWork = []string{"Verify the generated work and decide whether to send it to review, continue, or publish a handoff."}
		}
	}
	if listMissingOrPlaceholder(out.SuggestedNextSteps) || onlyGenericNextSteps(out.SuggestedNextSteps) {
		out.SuggestedNextSteps = fallback.SuggestedNextSteps
		if len(out.SuggestedNextSteps) == 0 {
			out.SuggestedNextSteps = []string{"Review the changed files and continue from the current task state."}
		}
	}
	if len(out.Assumptions) == 0 {
		out.Assumptions = fallback.Assumptions
	}
	if len(out.Risks) == 0 {
		out.Risks = fallback.Risks
	}
	if len(out.Dependencies) == 0 {
		out.Dependencies = fallback.Dependencies
	}
	if strings.TrimSpace(out.HandoffMessage) == "" || isHandoffPlaceholder(out.HandoffMessage) {
		out.HandoffMessage = fallback.HandoffMessage
		if out.HandoffMessage == "" {
			out.HandoffMessage = "Continue from the completed work and validation state recorded in this handoff."
		}
	}
	out = cleanHandoffPacketContent(out)
	return out
}

func cleanHandoffPacketContent(out HandoffPacketContent) HandoffPacketContent {
	out.CompletedWork = compactHandoffWorkItems(uniqueStrings(out.CompletedWork))
	out.ImportantDecisions = uniqueStrings(out.ImportantDecisions)
	out.ImplementationNotes = appendUsefulImplementationNotes(nil, uniqueStrings(out.ImplementationNotes)...)
	out.FilesComponentsAffected = uniqueStrings(out.FilesComponentsAffected)
	out.RemainingWork = latestActionableNextSteps(uniqueStrings(out.RemainingWork))
	out.SuggestedNextSteps = latestActionableNextSteps(uniqueStrings(out.SuggestedNextSteps))
	return out
}

func buildHandoffPacketFromCheckpoints(detail TaskDetail, checkpoints []HandoffCheckpoint) HandoffPacketContent {
	out := buildHandoffPacketContentFromDetail(detail)
	out.HandoffTimeline = nil
	out.CompletedWork = nil
	out.ImportantDecisions = nil
	out.RejectedApproaches = nil
	out.ArchitectureNotes = nil
	out.ImplementationNotes = nil
	out.FilesComponentsAffected = nil
	out.KnownIssues = nil
	out.FailedSessions = nil
	out.RemainingWork = nil
	out.SuggestedNextSteps = nil
	out.Assumptions = nil
	out.Risks = nil
	out.Dependencies = nil
	out.HandoffMessage = ""
	for _, checkpoint := range checkpoints {
		content := cleanHandoffPacketContent(checkpoint.Packet)
		checkpoint.Packet = content
		out.HandoffTimeline = append(out.HandoffTimeline, checkpointTimelineBlock(checkpoint))
		out.CompletedWork = appendUseful(out.CompletedWork, content.CompletedWork...)
		out.ImportantDecisions = appendUseful(out.ImportantDecisions, content.ImportantDecisions...)
		out.RejectedApproaches = appendUseful(out.RejectedApproaches, content.RejectedApproaches...)
		out.ArchitectureNotes = appendUseful(out.ArchitectureNotes, content.ArchitectureNotes...)
		out.ImplementationNotes = appendUsefulImplementationNotes(out.ImplementationNotes, content.ImplementationNotes...)
		out.FilesComponentsAffected = appendUseful(out.FilesComponentsAffected, content.FilesComponentsAffected...)
		out.KnownIssues = appendUseful(out.KnownIssues, content.KnownIssues...)
		out.FailedSessions = appendUseful(out.FailedSessions, content.FailedSessions...)
		out.Assumptions = appendUseful(out.Assumptions, content.Assumptions...)
		out.Risks = appendUseful(out.Risks, content.Risks...)
		out.Dependencies = appendUseful(out.Dependencies, content.Dependencies...)
		if strings.TrimSpace(content.CurrentState) != "" && !isHandoffPlaceholder(content.CurrentState) {
			out.CurrentState = content.CurrentState
		}
		if !listMissingOrPlaceholder(content.RemainingWork) {
			out.RemainingWork = content.RemainingWork
		}
		if !listMissingOrPlaceholder(content.SuggestedNextSteps) {
			out.SuggestedNextSteps = content.SuggestedNextSteps
		}
		if strings.TrimSpace(content.HandoffMessage) != "" && !isHandoffPlaceholder(content.HandoffMessage) {
			out.HandoffMessage = content.HandoffMessage
		}
	}
	fallback := buildHandoffPacketContentFromDetail(detail)
	if len(out.CompletedWork) == 0 {
		out.CompletedWork = fallback.CompletedWork
	}
	if len(out.ImportantDecisions) == 0 {
		out.ImportantDecisions = fallback.ImportantDecisions
	}
	if len(out.FilesComponentsAffected) == 0 {
		out.FilesComponentsAffected = fallback.FilesComponentsAffected
	}
	if len(out.SuggestedNextSteps) == 0 {
		out.SuggestedNextSteps = fallback.SuggestedNextSteps
	}
	if len(out.RemainingWork) == 0 && detail.Task.Status != "completed" {
		out.RemainingWork = []string{"Continue from the latest checkpoint and verify the current repository state."}
	}
	if out.HandoffMessage == "" {
		out.HandoffMessage = "Continue from the latest checkpoint in this handoff."
	}
	out.CompletedWork = limitStrings(uniqueStrings(out.CompletedWork), 30)
	out.ImportantDecisions = limitStrings(uniqueStrings(out.ImportantDecisions), 30)
	out.HandoffTimeline = limitStrings(uniqueStrings(out.HandoffTimeline), 30)
	out.RejectedApproaches = limitStrings(uniqueStrings(out.RejectedApproaches), 16)
	out.ArchitectureNotes = limitStrings(uniqueStrings(out.ArchitectureNotes), 20)
	out.ImplementationNotes = limitStrings(uniqueStrings(out.ImplementationNotes), 20)
	out.FilesComponentsAffected = limitStrings(uniqueStrings(out.FilesComponentsAffected), 30)
	out.KnownIssues = limitStrings(uniqueStrings(out.KnownIssues), 16)
	out.FailedSessions = limitStrings(uniqueStrings(out.FailedSessions), 8)
	out.RemainingWork = limitStrings(latestActionableNextSteps(uniqueStrings(out.RemainingWork)), 10)
	out.SuggestedNextSteps = limitStrings(latestActionableNextSteps(uniqueStrings(out.SuggestedNextSteps)), 10)
	out.Assumptions = limitStrings(uniqueStrings(out.Assumptions), 12)
	out.Risks = limitStrings(uniqueStrings(out.Risks), 16)
	out.Dependencies = limitStrings(uniqueStrings(out.Dependencies), 12)
	return out
}

func checkpointTimelineBlock(checkpoint HandoffCheckpoint) string {
	packet := cleanHandoffPacketContent(checkpoint.Packet)
	lines := []string{fmt.Sprintf("Checkpoint %d · %s · %s", checkpoint.Sequence, checkpoint.ActorID, checkpoint.CreatedAt.Format(time.RFC3339))}
	lines = appendTimelineSection(lines, "Completed work", packet.CompletedWork)
	lines = appendTimelineSection(lines, "Decisions", packet.ImportantDecisions)
	state := []string{}
	if packet.CurrentState != "" {
		state = append(state, packet.CurrentState)
	}
	state = append(state, packet.ImplementationNotes...)
	lines = appendTimelineSection(lines, "Current state / reasoning", state)
	lines = appendTimelineSection(lines, "Pending next steps at this checkpoint", packet.SuggestedNextSteps)
	return strings.Join(lines, "\n")
}

func renderSnapshotMarkdown(content SnapshotContent) string {
	var b strings.Builder
	b.WriteString("# Context Snapshot\n\n")
	writeMarkdownList(&b, "Recent Changes", content.RecentChanges)
	writeMarkdownList(&b, "Key Decisions", content.KeyDecisions)
	writeMarkdownList(&b, "Important Reasoning", content.Reasoning)
	writeMarkdownList(&b, "Open Questions", content.OpenQuestions)
	writeMarkdownText(&b, "Implementation Direction", content.ImplementationDirection)
	writeMarkdownList(&b, "Files / Components", content.FilesOrComponents)
	writeMarkdownList(&b, "Risks", content.Risks)
	writeMarkdownList(&b, "Blockers", content.Blockers)
	writeMarkdownList(&b, "Assumptions", content.Assumptions)
	writeMarkdownList(&b, "Next Recommended Actions", content.NextRecommendedActions)
	if extras := cleanStrings(content.ExtraSections); len(extras) > 0 {
		b.WriteString("## Extra Sections\n")
		b.WriteString(strings.Join(extras, "\n\n"))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String()) + "\n"
}

func renderHandoffMarkdown(content HandoffPacketContent) string {
	var b strings.Builder
	b.WriteString("# Task Handoff\n\n")
	writeMarkdownText(&b, "Objective", content.TaskObjective)
	writeMarkdownList(&b, "Original Requirements", content.OriginalRequirements)
	writeMarkdownText(&b, "Current Status", content.CurrentStatus)
	writeMarkdownText(&b, "Current State", content.CurrentState)
	writeMarkdownBlocks(&b, "Handoff Timeline", content.HandoffTimeline)
	writeMarkdownList(&b, "Completed Work", content.CompletedWork)
	writeMarkdownList(&b, "Important Decisions", content.ImportantDecisions)
	writeMarkdownList(&b, "Rejected Approaches", content.RejectedApproaches)
	writeMarkdownList(&b, "Architecture Notes", content.ArchitectureNotes)
	writeMarkdownList(&b, "Implementation Notes", content.ImplementationNotes)
	writeMarkdownList(&b, "Files / Components Affected", content.FilesComponentsAffected)
	writeMarkdownList(&b, "Known Issues", content.KnownIssues)
	writeMarkdownList(&b, "Failed / Interrupted Sessions", content.FailedSessions)
	writeMarkdownList(&b, "Remaining Work", content.RemainingWork)
	writeMarkdownList(&b, "Suggested Next Steps", content.SuggestedNextSteps)
	writeMarkdownList(&b, "Assumptions", content.Assumptions)
	writeMarkdownList(&b, "Risks", content.Risks)
	writeMarkdownList(&b, "Dependencies", content.Dependencies)
	writeMarkdownText(&b, "Handoff Message", content.HandoffMessage)
	if extras := cleanStrings(content.ExtraSections); len(extras) > 0 {
		b.WriteString("## Extra Sections\n")
		b.WriteString(strings.Join(extras, "\n\n"))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String()) + "\n"
}

type markdownValidationErrors []MarkdownValidationError

func (e markdownValidationErrors) Error() string {
	if len(e) == 0 {
		return "markdown validation failed"
	}
	return e[0].Message
}

func parseSnapshotMarkdown(markdown string) SnapshotContent {
	content, _ := parseSnapshotMarkdownStrict(markdown)
	return content
}

func parseSnapshotMarkdownStrict(markdown string) (SnapshotContent, error) {
	sections, err := parseMarkdownSectionsStrict(markdown, "Context Snapshot", snapshotMarkdownSections, nil)
	if err != nil {
		return SnapshotContent{}, err
	}
	return SnapshotContent{
		RecentChanges:           sectionsList(sections, "Recent Changes"),
		KeyDecisions:            sectionsList(sections, "Key Decisions"),
		Reasoning:               sectionsList(sections, "Important Reasoning"),
		OpenQuestions:           sectionsList(sections, "Open Questions"),
		ImplementationDirection: sectionsText(sections, "Implementation Direction"),
		FilesOrComponents:       sectionsList(sections, "Files / Components"),
		Risks:                   sectionsList(sections, "Risks"),
		Blockers:                sectionsList(sections, "Blockers"),
		Assumptions:             sectionsList(sections, "Assumptions"),
		NextRecommendedActions:  sectionsList(sections, "Next Recommended Actions"),
		ExtraSections:           unknownSections(sections, snapshotMarkdownSections),
	}, nil
}

func parseHandoffMarkdown(markdown string) HandoffPacketContent {
	content, _ := parseHandoffMarkdownStrict(markdown, false)
	return content
}

func parseHandoffMarkdownStrict(markdown string, publish bool) (HandoffPacketContent, error) {
	required := []string{}
	if publish {
		required = []string{"Objective", "Current Status"}
	}
	sections, err := parseMarkdownSectionsStrict(markdown, "Task Handoff", handoffMarkdownSections, required)
	if err != nil {
		return HandoffPacketContent{}, err
	}
	return HandoffPacketContent{
		TaskObjective:           sectionsText(sections, "Objective"),
		OriginalRequirements:    sectionsList(sections, "Original Requirements"),
		CurrentStatus:           sectionsText(sections, "Current Status"),
		CurrentState:            sectionsText(sections, "Current State"),
		HandoffTimeline:         sectionsBlocks(sections, "Handoff Timeline"),
		CompletedWork:           sectionsList(sections, "Completed Work"),
		ImportantDecisions:      sectionsList(sections, "Important Decisions"),
		RejectedApproaches:      sectionsList(sections, "Rejected Approaches"),
		ArchitectureNotes:       sectionsList(sections, "Architecture Notes"),
		ImplementationNotes:     sectionsList(sections, "Implementation Notes"),
		FilesComponentsAffected: sectionsList(sections, "Files / Components Affected"),
		KnownIssues:             sectionsList(sections, "Known Issues"),
		FailedSessions:          sectionsList(sections, "Failed / Interrupted Sessions"),
		RemainingWork:           sectionsList(sections, "Remaining Work"),
		SuggestedNextSteps:      sectionsList(sections, "Suggested Next Steps"),
		Assumptions:             sectionsList(sections, "Assumptions"),
		Risks:                   sectionsList(sections, "Risks"),
		Dependencies:            sectionsList(sections, "Dependencies"),
		HandoffMessage:          sectionsText(sections, "Handoff Message"),
		ExtraSections:           unknownSections(sections, handoffMarkdownSections),
	}, nil
}

func parseMarkdownSections(markdown string) map[string]string {
	sections, _ := parseMarkdownSectionsStrict(markdown, "", nil, nil)
	return sections
}

func parseMarkdownSectionsStrict(markdown, title string, known []struct {
	Title string
	Key   string
}, required []string) (map[string]string, error) {
	out := map[string]string{}
	var errs markdownValidationErrors
	knownTitles := map[string]bool{}
	for _, item := range known {
		knownTitles[item.Title] = true
	}
	current := ""
	lines := strings.Split(markdown, "\n")
	var b strings.Builder
	flush := func() {
		if current != "" {
			out[current] = strings.TrimSpace(b.String())
			b.Reset()
		}
	}
	seenTitle := title == ""
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if current == "" {
				errs = append(errs, MarkdownValidationError{Line: i + 1, Message: "section title cannot be empty"})
				continue
			}
			if _, exists := out[current]; exists {
				errs = append(errs, MarkdownValidationError{Section: current, Line: i + 1, Message: "duplicate section"})
			}
			continue
		}
		if strings.HasPrefix(line, "# ") {
			heading := strings.TrimSpace(strings.TrimPrefix(line, "# "))
			if title != "" && heading != title && !isAllowedMarkdownTitleAlias(title, heading) {
				errs = append(errs, MarkdownValidationError{Line: i + 1, Message: fmt.Sprintf("expected top-level heading '# %s'", title)})
			}
			seenTitle = true
			continue
		}
		if current != "" {
			if len(knownTitles) > 0 && !knownTitles[current] && current != "Extra Sections" {
				errs = append(errs, MarkdownValidationError{Section: current, Line: i + 1, Message: "unknown section must be moved under Extra Sections"})
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	flush()
	if !seenTitle {
		errs = append(errs, MarkdownValidationError{Line: 1, Message: fmt.Sprintf("missing top-level heading '# %s'", title)})
	}
	for _, section := range required {
		if strings.TrimSpace(out[section]) == "" || strings.TrimSpace(out[section]) == "Not specified." || strings.TrimSpace(out[section]) == "- None recorded." {
			errs = append(errs, MarkdownValidationError{Section: section, Message: "required section is empty"})
		}
	}
	if len(errs) > 0 {
		return out, errs
	}
	return out, nil
}

func isAllowedMarkdownTitleAlias(expected, got string) bool {
	return expected == "Task Handoff" && got == "TaskPilot Handoff"
}

func sectionsText(sections map[string]string, key string) string {
	value := strings.TrimSpace(sections[key])
	if value == "Not specified." {
		return ""
	}
	return value
}

func sectionsList(sections map[string]string, key string) []string {
	body := strings.TrimSpace(sections[key])
	if body == "" {
		return nil
	}
	out := []string{}
	current := ""
	flush := func() {
		item := strings.TrimSpace(current)
		current = ""
		if item == "" || item == "None recorded." {
			return
		}
		out = append(out, item)
	}
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line == "None recorded." || line == "- None recorded." {
			continue
		}
		if strings.HasPrefix(raw, "- ") {
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "- "))
			continue
		}
		if strings.HasPrefix(line, "- ") && current != "" {
			current = strings.TrimSpace(current + "; " + strings.TrimSpace(strings.TrimPrefix(line, "- ")))
			continue
		}
		if current == "" {
			current = line
		} else {
			current = strings.TrimSpace(current + " " + line)
		}
	}
	flush()
	return uniqueStrings(out)
}

func sectionsBlocks(sections map[string]string, key string) []string {
	body := strings.TrimSpace(sections[key])
	if body == "" || body == "None recorded." || body == "- None recorded." {
		return nil
	}
	parts := strings.Split(body, "\n\n")
	out := []string{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "None recorded." || part == "- None recorded." {
			continue
		}
		out = append(out, strings.TrimPrefix(part, "- "))
	}
	return uniqueStrings(out)
}

func unknownSections(sections map[string]string, known []struct {
	Title string
	Key   string
}) []string {
	allowed := map[string]bool{}
	for _, item := range known {
		allowed[item.Title] = true
	}
	allowed["Extra Sections"] = true
	out := []string{}
	if body := strings.TrimSpace(sections["Extra Sections"]); body != "" {
		out = append(out, body)
	}
	for title, body := range sections {
		if allowed[title] || strings.TrimSpace(body) == "" {
			continue
		}
		out = append(out, "## "+title+"\n"+strings.TrimSpace(body))
	}
	return out
}

func writeMarkdownText(b *strings.Builder, title, body string) {
	b.WriteString("## ")
	b.WriteString(title)
	b.WriteString("\n")
	body = strings.TrimSpace(body)
	if body == "" {
		b.WriteString("Not specified.\n\n")
		return
	}
	b.WriteString(body)
	b.WriteString("\n\n")
}

func writeMarkdownList(b *strings.Builder, title string, values []string) {
	b.WriteString("## ")
	b.WriteString(title)
	b.WriteString("\n")
	values = uniqueStrings(values)
	if len(values) == 0 {
		b.WriteString("- None recorded.\n\n")
		return
	}
	for _, value := range values {
		b.WriteString("- ")
		b.WriteString(strings.TrimSpace(value))
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func writeMarkdownBlocks(b *strings.Builder, title string, values []string) {
	b.WriteString("## ")
	b.WriteString(title)
	b.WriteString("\n")
	values = uniqueStrings(values)
	if len(values) == 0 {
		b.WriteString("None recorded.\n\n")
		return
	}
	for _, value := range values {
		b.WriteString(strings.TrimSpace(value))
		b.WriteString("\n\n")
	}
}

func formatDecision(d DecisionRecord) string {
	parts := []string{d.Decision}
	if d.Reason != "" {
		parts = append(parts, "Reason: "+d.Reason)
	}
	if d.Impact != "" {
		parts = append(parts, "Impact: "+d.Impact)
	}
	if len(d.Alternatives) > 0 {
		parts = append(parts, "Alternatives: "+strings.Join(d.Alternatives, ", "))
	}
	return strings.Join(parts, " | ")
}

func inferImplementationDirection(detail TaskDetail, content SnapshotContent) string {
	if len(content.NextRecommendedActions) > 0 {
		return content.NextRecommendedActions[len(content.NextRecommendedActions)-1]
	}
	if len(content.RecentChanges) > 0 {
		return content.RecentChanges[len(content.RecentChanges)-1]
	}
	if detail.Task.Status == "completed" {
		return "Task appears completed; verify final outputs and close any follow-up work."
	}
	return "Continue toward the task goal using the latest decisions, risks, blockers, and outputs."
}

func inferHandoffCurrentState(detail TaskDetail) string {
	if detail.Task.Status == "completed" {
		return "Task is marked completed; verify final outputs before reopening or continuing."
	}
	if detail.Task.Status == "blocked" && len(detail.Task.Blockers) > 0 {
		return "Task is blocked: " + strings.Join(detail.Task.Blockers, "; ")
	}
	for i := len(detail.Context) - 1; i >= 0; i-- {
		entry := detail.Context[i]
		if entry.Kind == "summary" && !isNoisyContext(entry.Content) {
			return strings.TrimSpace(entry.Content)
		}
	}
	return "Task is " + detail.Task.Status + "; continue from the latest task memory and verify the current repository state."
}

func validateHandoffQuality(content HandoffPacketContent) []MarkdownValidationError {
	errs := []MarkdownValidationError{}
	if strings.TrimSpace(content.TaskObjective) == "" {
		errs = append(errs, MarkdownValidationError{Section: "Objective", Message: "objective is required"})
	}
	if strings.TrimSpace(content.CurrentStatus) == "" {
		errs = append(errs, MarkdownValidationError{Section: "Current Status", Message: "current status is required"})
	}
	if strings.TrimSpace(content.CurrentState) == "" || isHandoffPlaceholder(content.CurrentState) {
		errs = append(errs, MarkdownValidationError{Section: "Current State", Message: "current state is required"})
	}
	if listMissingOrPlaceholder(content.CompletedWork) {
		errs = append(errs, MarkdownValidationError{Section: "Completed Work", Message: "completed work is required; list what was actually done"})
	}
	if !hasDecisionState(content.ImportantDecisions) {
		errs = append(errs, MarkdownValidationError{Section: "Important Decisions", Message: "important decisions are required, or explicitly state: No material decision made; work followed existing requirements."})
	}
	if listMissingOrPlaceholder(content.RemainingWork) {
		errs = append(errs, MarkdownValidationError{Section: "Remaining Work", Message: "remaining work is required; use an explicit completion statement if nothing remains"})
	}
	if listMissingOrPlaceholder(content.SuggestedNextSteps) {
		errs = append(errs, MarkdownValidationError{Section: "Suggested Next Steps", Message: "suggested next steps are required"})
	}
	if strings.TrimSpace(content.HandoffMessage) == "" || isHandoffPlaceholder(content.HandoffMessage) {
		errs = append(errs, MarkdownValidationError{Section: "Handoff Message", Message: "handoff message is required"})
	}
	return errs
}

func hasDecisionState(decisions []string) bool {
	for _, decision := range decisions {
		normalized := strings.ToLower(strings.TrimSpace(decision))
		if normalized == "" {
			continue
		}
		if isHandoffPlaceholder(normalized) {
			continue
		}
		if strings.Contains(normalized, "no material decision made") {
			return true
		}
		return true
	}
	return false
}

func listMissingOrPlaceholder(items []string) bool {
	items = cleanStrings(items)
	if len(items) == 0 {
		return true
	}
	for _, item := range items {
		if !isHandoffPlaceholder(item) {
			return false
		}
	}
	return true
}

func isHandoffPlaceholder(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "replace this ") || strings.Contains(value, "write a concise message")
}

func allPlaceholders(items []string) bool {
	items = cleanStrings(items)
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if !isHandoffPlaceholder(item) {
			return false
		}
	}
	return true
}

func onlyGenericNextSteps(items []string) bool {
	items = cleanStrings(items)
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		normalized := strings.ToLower(strings.TrimSpace(item))
		if normalized != "continue from the latest task context and verify completion criteria." &&
			normalized != "review the changed files and continue from the current task state." {
			return false
		}
	}
	return true
}

func handoffSupportingEvidence(content HandoffPacketContent) []string {
	out := []string{}
	for _, item := range content.FilesComponentsAffected {
		out = appendUseful(out, "File/component: "+item)
	}
	for _, item := range content.HandoffTimeline {
		out = appendUseful(out, "Timeline: "+firstLine(item))
	}
	for _, item := range content.KnownIssues {
		out = appendUseful(out, "Known issue: "+item)
	}
	for _, item := range content.Risks {
		out = appendUseful(out, "Risk: "+item)
	}
	return limitStrings(uniqueStrings(out), 20)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		return strings.TrimSpace(value[:idx])
	}
	return value
}

func isNoisyContext(content string) bool {
	content = strings.ToLower(strings.TrimSpace(content))
	noisy := []string{
		"taskpilot run is still active; heartbeat renewed",
		"taskpilot run started agent command",
		"agent command completed successfully through taskpilot run",
		"direction: taskpilot run started agent command",
	}
	for _, item := range noisy {
		if strings.Contains(content, item) {
			return true
		}
	}
	return false
}

func appendUseful(out []string, values ...string) []string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || isNoisyContext(value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func appendUsefulImplementationNotes(out []string, values ...string) []string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !isUsefulImplementationNote(value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func isUsefulImplementationNote(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || isNoisyContext(value) || len(value) < 12 || strings.HasPrefix(value, ",") {
		return false
	}
	return strings.Contains(value, ":")
}

func compactHandoffWorkItems(values []string) []string {
	out := []string{}
	current := ""
	flush := func() {
		current = strings.TrimSpace(current)
		if current != "" {
			out = append(out, current)
		}
		current = ""
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if current != "" && isNestedWorkFragment(value) {
			current += "; " + value
			continue
		}
		flush()
		current = value
	}
	flush()
	return uniqueStrings(out)
}

func isNestedWorkFragment(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, ":") || strings.HasSuffix(value, ".") {
		return false
	}
	r := []rune(value)
	if len(r) == 0 {
		return false
	}
	return r[0] >= 'a' && r[0] <= 'z'
}

func latestHandoffForTimeline(handoffs []Handoff) *Handoff {
	if len(handoffs) == 0 {
		return nil
	}
	latest := handoffs[0]
	for _, handoff := range handoffs[1:] {
		if handoff.CreatedAt.After(latest.CreatedAt) {
			latest = handoff
		}
	}
	return &latest
}

func buildHandoffTimeline(detail TaskDetail) []string {
	if len(detail.Handoffs) == 0 {
		return nil
	}
	handoffs := append([]Handoff(nil), detail.Handoffs...)
	sort.Slice(handoffs, func(i, j int) bool {
		return handoffs[i].CreatedAt.Before(handoffs[j].CreatedAt)
	})
	out := []string{}
	for i, handoff := range handoffs {
		start := timeFloor{}
		if i > 0 {
			start.t = handoffs[i-1].CreatedAt
			start.ok = true
		}
		block := handoffTimelineBlock(i+1, handoff, detail.Task.Goal, entriesBetween(detail.Context, start, handoff.CreatedAt))
		if block != "" {
			out = append(out, block)
		}
	}
	return out
}

type timeFloor struct {
	t  time.Time
	ok bool
}

func entriesBetween(entries []ContextEntry, start timeFloor, end time.Time) []ContextEntry {
	out := []ContextEntry{}
	for _, entry := range entries {
		if start.ok && !entry.CreatedAt.After(start.t) {
			continue
		}
		if entry.CreatedAt.After(end) {
			continue
		}
		if isNoisyContext(entry.Content) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func handoffTimelineBlock(n int, handoff Handoff, taskGoal string, entries []ContextEntry) string {
	context := []string{}
	completed := []string{}
	decisions := []string{}
	reasoning := []string{}
	open := []string{}
	for _, entry := range entries {
		switch entry.Kind {
		case "summary":
			completed = appendUseful(completed, entry.Content)
		case "decision":
			decisions = appendUseful(decisions, entry.Content)
		case "note":
			reasoning = appendUseful(reasoning, entry.Content)
		case "risk":
			open = appendUseful(open, entry.Content)
		case "blocker":
			if isFailedRunContext(entry.Content) {
				reasoning = appendUseful(reasoning, normalizeFailedRunContext(entry.Content))
			} else {
				open = appendUseful(open, entry.Content)
			}
		case "next":
			open = appendUseful(open, entry.Content)
		case "output_ref":
			context = appendUseful(context, conciseOutputRef(entry.Content))
		}
	}
	if isUsefulHandoffMessage(handoff.ResumeSummary, taskGoal) {
		context = appendUseful(context, handoff.ResumeSummary)
	}
	open = appendUseful(open, handoff.NextSteps...)
	lines := []string{fmt.Sprintf("Handoff %d · %s · %s", n, handoff.Status, handoff.CreatedAt.Format(time.RFC3339))}
	lines = appendTimelineSection(lines, "Context", context)
	lines = appendTimelineSection(lines, "Work completed", completed)
	lines = appendTimelineSection(lines, "Decisions made", decisions)
	lines = appendTimelineSection(lines, "Reasoning", reasoning)
	lines = appendTimelineSection(lines, "Open items from that handoff", open)
	return strings.Join(lines, "\n")
}

func appendTimelineSection(lines []string, title string, values []string) []string {
	values = limitStrings(uniqueStrings(values), 5)
	if len(values) == 0 {
		return lines
	}
	lines = append(lines, title+":")
	for _, value := range values {
		lines = append(lines, "- "+value)
	}
	return lines
}

func latestActionableNextSteps(values []string) []string {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || isNoisyContext(value) || !isActionableNextStep(value) {
			continue
		}
		out = append(out, value)
	}
	return uniqueStrings(out)
}

func isActionableNextStep(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return false
	}
	nonActionable := []string{
		"if further refinement is needed",
		"if desired",
		"if needed",
		"if further work is needed",
		"no further action",
		"nothing else",
		"task appears completed",
	}
	for _, phrase := range nonActionable {
		if strings.Contains(lower, phrase) {
			return false
		}
	}
	return true
}

func isFailedRunContext(content string) bool {
	return strings.Contains(strings.ToLower(content), "taskpilot run command failed")
}

func normalizeFailedRunContext(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(content), "failed run:") {
		return content
	}
	return "Failed run: " + content
}

func isUsefulHandoffMessage(message, goal string) bool {
	message = strings.TrimSpace(message)
	if message == "" || isNoisyContext(message) {
		return false
	}
	normalizedMessage := normalizeComparableText(message)
	normalizedGoal := normalizeComparableText(goal)
	if normalizedGoal != "" && normalizedMessage == normalizedGoal {
		return false
	}
	return true
}

func normalizeComparableText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func conciseOutputRef(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	header := strings.ToLower(lines[0])
	if strings.Contains(header, "files changed during this run") {
		files := []string{}
		for _, raw := range lines[1:] {
			line := strings.TrimSpace(raw)
			if strings.HasPrefix(line, "- ") {
				files = append(files, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
			}
		}
		if len(files) == 0 {
			return ""
		}
		return strings.Join(uniqueStrings(files), ", ")
	}
	if !strings.Contains(header, "touched files detected by git status after taskpilot run") {
		return content
	}
	files := []string{}
	inNew := false
	inExisting := false
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		switch strings.ToLower(line) {
		case "newly changed during run:":
			inNew = true
			inExisting = false
			continue
		case "already changed before or still changed after run:", "pre-existing dirty worktree files:":
			inNew = false
			inExisting = true
			continue
		}
		if strings.HasPrefix(line, "- ") && inNew && !inExisting {
			files = append(files, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
		}
	}
	if len(files) == 0 {
		return ""
	}
	return "Files changed during this run: " + strings.Join(uniqueStrings(files), ", ")
}
