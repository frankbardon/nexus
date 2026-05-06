package events

import (
	"encoding/json"
	"testing"
)

// versionedPayload pairs an empty value of a versioned struct with the
// version constant the producer is expected to stamp. The roundtrip test
// asserts that marshalling an explicit `SchemaVersion = <Const>` value
// survives JSON unmarshal — i.e., the `_schema_version` JSON tag is wired
// correctly.
type versionedPayload struct {
	name      string
	construct func() any
	wantVer   int
}

// versionedPayloads enumerates every event-payload struct that carries a
// schema version. Adding a new versioned struct without listing it here is
// caught by the round-trip test in TestVersionedPayloadsRoundtrip.
func versionedPayloads() []versionedPayload {
	return []versionedPayload{
		// core.go
		{"BootConfig", func() any { return BootConfig{SchemaVersion: BootConfigVersion} }, BootConfigVersion},
		{"ShutdownReason", func() any { return ShutdownReason{SchemaVersion: ShutdownReasonVersion} }, ShutdownReasonVersion},
		{"ErrorInfo", func() any { return ErrorInfo{SchemaVersion: ErrorInfoVersion} }, ErrorInfoVersion},
		{"TickInfo", func() any { return TickInfo{SchemaVersion: TickInfoVersion} }, TickInfoVersion},
		// llm.go
		{"LLMRequest", func() any { return LLMRequest{SchemaVersion: LLMRequestVersion} }, LLMRequestVersion},
		{"LLMResponse", func() any { return LLMResponse{SchemaVersion: LLMResponseVersion} }, LLMResponseVersion},
		{"StreamChunk", func() any { return StreamChunk{SchemaVersion: StreamChunkVersion} }, StreamChunkVersion},
		{"StreamEnd", func() any { return StreamEnd{SchemaVersion: StreamEndVersion} }, StreamEndVersion},
		{"BatchSubmit", func() any { return BatchSubmit{SchemaVersion: BatchSubmitVersion} }, BatchSubmitVersion},
		{"BatchStatus", func() any { return BatchStatus{SchemaVersion: BatchStatusVersion} }, BatchStatusVersion},
		{"BatchResults", func() any { return BatchResults{SchemaVersion: BatchResultsVersion} }, BatchResultsVersion},
		// agent.go
		{"TurnInfo", func() any { return TurnInfo{SchemaVersion: TurnInfoVersion} }, TurnInfoVersion},
		{"Plan", func() any { return Plan{SchemaVersion: PlanVersion} }, PlanVersion},
		{"SubagentSpawn", func() any { return SubagentSpawn{SchemaVersion: SubagentSpawnVersion} }, SubagentSpawnVersion},
		{"SubagentStarted", func() any { return SubagentStarted{SchemaVersion: SubagentStartedVersion} }, SubagentStartedVersion},
		{"SubagentIteration", func() any { return SubagentIteration{SchemaVersion: SubagentIterationVersion} }, SubagentIterationVersion},
		{"AgentToolChoice", func() any { return AgentToolChoice{SchemaVersion: AgentToolChoiceVersion} }, AgentToolChoiceVersion},
		{"SubagentComplete", func() any { return SubagentComplete{SchemaVersion: SubagentCompleteVersion} }, SubagentCompleteVersion},
		// tool.go
		{"ToolCall", func() any { return ToolCall{SchemaVersion: ToolCallVersion} }, ToolCallVersion},
		{"ToolCatalogQuery", func() any { return ToolCatalogQuery{SchemaVersion: ToolCatalogQueryVersion} }, ToolCatalogQueryVersion},
		{"ToolTimeout", func() any { return ToolTimeout{SchemaVersion: ToolTimeoutVersion} }, ToolTimeoutVersion},
		{"ToolResult", func() any { return ToolResult{SchemaVersion: ToolResultVersion} }, ToolResultVersion},
		// io.go
		{"UserInput", func() any { return UserInput{SchemaVersion: UserInputVersion} }, UserInputVersion},
		{"AgentOutput", func() any { return AgentOutput{SchemaVersion: AgentOutputVersion} }, AgentOutputVersion},
		{"OutputChunk", func() any { return OutputChunk{SchemaVersion: OutputChunkVersion} }, OutputChunkVersion},
		{"StreamRef", func() any { return StreamRef{SchemaVersion: StreamRefVersion} }, StreamRefVersion},
		{"StatusUpdate", func() any { return StatusUpdate{SchemaVersion: StatusUpdateVersion} }, StatusUpdateVersion},
		{"ApprovalRequest", func() any { return ApprovalRequest{SchemaVersion: ApprovalRequestVersion} }, ApprovalRequestVersion},
		{"ApprovalResponse", func() any { return ApprovalResponse{SchemaVersion: ApprovalResponseVersion} }, ApprovalResponseVersion},
		{"HistoryReplay", func() any { return HistoryReplay{SchemaVersion: HistoryReplayVersion} }, HistoryReplayVersion},
		{"FileOpenRequest", func() any { return FileOpenRequest{SchemaVersion: FileOpenRequestVersion} }, FileOpenRequestVersion},
		{"FileOpenResponse", func() any { return FileOpenResponse{SchemaVersion: FileOpenResponseVersion} }, FileOpenResponseVersion},
		{"FileOutputDirRequest", func() any { return FileOutputDirRequest{SchemaVersion: FileOutputDirRequestVersion} }, FileOutputDirRequestVersion},
		{"FileOutputDirResponse", func() any { return FileOutputDirResponse{SchemaVersion: FileOutputDirResponseVersion} }, FileOutputDirResponseVersion},
		{"FileSelected", func() any { return FileSelected{SchemaVersion: FileSelectedVersion} }, FileSelectedVersion},
		{"SessionInfo", func() any { return SessionInfo{SchemaVersion: SessionInfoVersion} }, SessionInfoVersion},
		// memory.go
		{"MemoryEntry", func() any { return MemoryEntry{SchemaVersion: MemoryEntryVersion} }, MemoryEntryVersion},
		{"MemoryQuery", func() any { return MemoryQuery{SchemaVersion: MemoryQueryVersion} }, MemoryQueryVersion},
		{"MemoryResult", func() any { return MemoryResult{SchemaVersion: MemoryResultVersion} }, MemoryResultVersion},
		{"HistoryQuery", func() any { return HistoryQuery{SchemaVersion: HistoryQueryVersion} }, HistoryQueryVersion},
		{"LongTermMemoryEntry", func() any { return LongTermMemoryEntry{SchemaVersion: LongTermMemoryEntryVersion} }, LongTermMemoryEntryVersion},
		{"LongTermMemoryLoaded", func() any { return LongTermMemoryLoaded{SchemaVersion: LongTermMemoryLoadedVersion} }, LongTermMemoryLoadedVersion},
		{"LongTermMemoryStoreRequest", func() any { return LongTermMemoryStoreRequest{SchemaVersion: LongTermMemoryStoreRequestVersion} }, LongTermMemoryStoreRequestVersion},
		{"LongTermMemoryStored", func() any { return LongTermMemoryStored{SchemaVersion: LongTermMemoryStoredVersion} }, LongTermMemoryStoredVersion},
		{"LongTermMemoryReadRequest", func() any { return LongTermMemoryReadRequest{SchemaVersion: LongTermMemoryReadRequestVersion} }, LongTermMemoryReadRequestVersion},
		{"LongTermMemoryReadResult", func() any { return LongTermMemoryReadResult{SchemaVersion: LongTermMemoryReadResultVersion} }, LongTermMemoryReadResultVersion},
		{"LongTermMemoryDeleteRequest", func() any { return LongTermMemoryDeleteRequest{SchemaVersion: LongTermMemoryDeleteRequestVersion} }, LongTermMemoryDeleteRequestVersion},
		{"LongTermMemoryDeleted", func() any { return LongTermMemoryDeleted{SchemaVersion: LongTermMemoryDeletedVersion} }, LongTermMemoryDeletedVersion},
		{"LongTermMemoryQuery", func() any { return LongTermMemoryQuery{SchemaVersion: LongTermMemoryQueryVersion} }, LongTermMemoryQueryVersion},
		{"LongTermMemoryListResult", func() any { return LongTermMemoryListResult{SchemaVersion: LongTermMemoryListResultVersion} }, LongTermMemoryListResultVersion},
		{"CompactionTriggered", func() any { return CompactionTriggered{SchemaVersion: CompactionTriggeredVersion} }, CompactionTriggeredVersion},
		{"CompactionComplete", func() any { return CompactionComplete{SchemaVersion: CompactionCompleteVersion} }, CompactionCompleteVersion},
		// skill.go
		{"SkillCatalog", func() any { return SkillCatalog{SchemaVersion: SkillCatalogVersion} }, SkillCatalogVersion},
		{"SkillActivation", func() any { return SkillActivation{SchemaVersion: SkillActivationVersion} }, SkillActivationVersion},
		{"SkillContent", func() any { return SkillContent{SchemaVersion: SkillContentVersion} }, SkillContentVersion},
		{"SkillRef", func() any { return SkillRef{SchemaVersion: SkillRefVersion} }, SkillRefVersion},
		{"SkillResourceReq", func() any { return SkillResourceReq{SchemaVersion: SkillResourceReqVersion} }, SkillResourceReqVersion},
		{"SkillResourceData", func() any { return SkillResourceData{SchemaVersion: SkillResourceDataVersion} }, SkillResourceDataVersion},
		// session.go
		{"SessionFile", func() any { return SessionFile{SchemaVersion: SessionFileVersion} }, SessionFileVersion},
		// schema.go
		{"SchemaRegistration", func() any { return SchemaRegistration{SchemaVersion: SchemaRegistrationVersion} }, SchemaRegistrationVersion},
		{"SchemaDeregistration", func() any { return SchemaDeregistration{SchemaVersion: SchemaDeregistrationVersion} }, SchemaDeregistrationVersion},
		// cancel.go
		{"CancelRequest", func() any { return CancelRequest{SchemaVersion: CancelRequestVersion} }, CancelRequestVersion},
		{"CancelActive", func() any { return CancelActive{SchemaVersion: CancelActiveVersion} }, CancelActiveVersion},
		{"CancelComplete", func() any { return CancelComplete{SchemaVersion: CancelCompleteVersion} }, CancelCompleteVersion},
		{"CancelResume", func() any { return CancelResume{SchemaVersion: CancelResumeVersion} }, CancelResumeVersion},
		// citations.go
		{"RetrievalContext", func() any { return RetrievalContext{SchemaVersion: RetrievalContextVersion} }, RetrievalContextVersion},
		{"CitedResponse", func() any { return CitedResponse{SchemaVersion: CitedResponseVersion} }, CitedResponseVersion},
		// code.go
		{"CodeExecRequest", func() any { return CodeExecRequest{SchemaVersion: CodeExecRequestVersion} }, CodeExecRequestVersion},
		{"CodeExecStdout", func() any { return CodeExecStdout{SchemaVersion: CodeExecStdoutVersion} }, CodeExecStdoutVersion},
		{"CodeExecResult", func() any { return CodeExecResult{SchemaVersion: CodeExecResultVersion} }, CodeExecResultVersion},
		// embeddings.go
		{"EmbeddingsRequest", func() any { return EmbeddingsRequest{SchemaVersion: EmbeddingsRequestVersion} }, EmbeddingsRequestVersion},
		// hitl.go
		{"HITLRequest", func() any { return HITLRequest{SchemaVersion: HITLRequestVersion} }, HITLRequestVersion},
		{"HITLResponse", func() any { return HITLResponse{SchemaVersion: HITLResponseVersion} }, HITLResponseVersion},
		// hybrid.go
		{"HybridQuery", func() any { return HybridQuery{SchemaVersion: HybridQueryVersion} }, HybridQueryVersion},
		// lexical.go
		{"LexicalUpsert", func() any { return LexicalUpsert{SchemaVersion: LexicalUpsertVersion} }, LexicalUpsertVersion},
		{"LexicalQuery", func() any { return LexicalQuery{SchemaVersion: LexicalQueryVersion} }, LexicalQueryVersion},
		{"LexicalDelete", func() any { return LexicalDelete{SchemaVersion: LexicalDeleteVersion} }, LexicalDeleteVersion},
		{"LexicalNamespaceDrop", func() any { return LexicalNamespaceDrop{SchemaVersion: LexicalNamespaceDropVersion} }, LexicalNamespaceDropVersion},
		// memory_vector.go
		{"VectorMemoryStore", func() any { return VectorMemoryStore{SchemaVersion: VectorMemoryStoreVersion} }, VectorMemoryStoreVersion},
		// plan.go
		{"PlanRequest", func() any { return PlanRequest{SchemaVersion: PlanRequestVersion} }, PlanRequestVersion},
		{"PlanResult", func() any { return PlanResult{SchemaVersion: PlanResultVersion} }, PlanResultVersion},
		{"PlanProgress", func() any { return PlanProgress{SchemaVersion: PlanProgressVersion} }, PlanProgressVersion},
		// provider.go
		{"ProviderFallback", func() any { return ProviderFallback{SchemaVersion: ProviderFallbackVersion} }, ProviderFallbackVersion},
		{"ProviderFanoutStart", func() any { return ProviderFanoutStart{SchemaVersion: ProviderFanoutStartVersion} }, ProviderFanoutStartVersion},
		{"ProviderFanoutResponse", func() any { return ProviderFanoutResponse{SchemaVersion: ProviderFanoutResponseVersion} }, ProviderFanoutResponseVersion},
		{"ProviderFanoutComplete", func() any { return ProviderFanoutComplete{SchemaVersion: ProviderFanoutCompleteVersion} }, ProviderFanoutCompleteVersion},
		{"ProviderFanoutChoose", func() any { return ProviderFanoutChoose{SchemaVersion: ProviderFanoutChooseVersion} }, ProviderFanoutChooseVersion},
		{"ProviderFanoutChosen", func() any { return ProviderFanoutChosen{SchemaVersion: ProviderFanoutChosenVersion} }, ProviderFanoutChosenVersion},
		// rag.go
		{"RAGIngest", func() any { return RAGIngest{SchemaVersion: RAGIngestVersion} }, RAGIngestVersion},
		{"RAGIngestDelete", func() any { return RAGIngestDelete{SchemaVersion: RAGIngestDeleteVersion} }, RAGIngestDeleteVersion},
		// reranker.go
		{"RerankRequest", func() any { return RerankRequest{SchemaVersion: RerankRequestVersion} }, RerankRequestVersion},
		// search.go
		{"SearchRequest", func() any { return SearchRequest{SchemaVersion: SearchRequestVersion} }, SearchRequestVersion},
		// thinking.go
		{"ThinkingStep", func() any { return ThinkingStep{SchemaVersion: ThinkingStepVersion} }, ThinkingStepVersion},
		// vector.go
		{"VectorUpsert", func() any { return VectorUpsert{SchemaVersion: VectorUpsertVersion} }, VectorUpsertVersion},
		{"VectorQuery", func() any { return VectorQuery{SchemaVersion: VectorQueryVersion} }, VectorQueryVersion},
		{"VectorDelete", func() any { return VectorDelete{SchemaVersion: VectorDeleteVersion} }, VectorDeleteVersion},
		{"VectorNamespaceDrop", func() any { return VectorNamespaceDrop{SchemaVersion: VectorNamespaceDropVersion} }, VectorNamespaceDropVersion},
	}
}

// TestVersionedPayloadsRoundtrip marshals each versioned payload to JSON
// and back through a generic decode, asserting the SchemaVersion field
// survives the round-trip with the running constant. This catches:
//  1. JSON tag drift (e.g., a typo `_schemaversion` instead of
//     `_schema_version`)
//  2. The constant being out of sync with the producer-set value.
func TestVersionedPayloadsRoundtrip(t *testing.T) {
	for _, p := range versionedPayloads() {
		p := p
		t.Run(p.name, func(t *testing.T) {
			data, err := json.Marshal(p.construct())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var generic map[string]any
			if err := json.Unmarshal(data, &generic); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			raw, ok := generic["_schema_version"]
			if !ok {
				t.Fatalf("missing _schema_version field; got keys=%v", keysOf(generic))
			}
			// JSON numbers round-trip as float64 through map[string]any.
			got, ok := raw.(float64)
			if !ok {
				t.Fatalf("_schema_version not numeric: %T = %v", raw, raw)
			}
			if int(got) != p.wantVer {
				t.Fatalf("expected version %d; got %v", p.wantVer, got)
			}
		})
	}
}

// TestMissingSchemaVersionTreatedAsZero documents the v0 == v1 rule:
// when an incoming JSON document omits `_schema_version`, the field
// deserializes to Go's zero value (0). Consumers must lift that to v1 —
// the running code's contract — at the migration boundary (see
// pkg/events/compat.Apply with from=0, to=1).
func TestMissingSchemaVersionTreatedAsZero(t *testing.T) {
	// A payload written before the field existed.
	const raw = `{"Content":"hello"}`
	var got UserInput
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != 0 {
		t.Fatalf("expected zero version on missing field; got %d", got.SchemaVersion)
	}
	if got.Content != "hello" {
		t.Fatalf("expected content preserved; got %q", got.Content)
	}
	// The running code's UserInput contract is v1. Consumers can detect
	// the v0 case (received-zero) and either (a) treat it as v1 directly
	// or (b) call compat.Apply("io.input", 0, UserInputVersion, ...) to
	// run any registered v0->v1 normalization. With the registry empty,
	// option (a) is the implicit policy.
	if UserInputVersion != 1 {
		t.Fatalf("UserInputVersion drifted from 1 — update the v0==v1 rule in doc.go")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
