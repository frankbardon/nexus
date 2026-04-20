package codeexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/traefik/yaegi/interp"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// toolResult is the struct scripts receive from tools.* calls. Mirrors
// events.ToolResult but exposes only the fields scripts can reason about.
// Surfaced in Yaegi as tools.Result.
type toolResult struct {
	Output     string `json:"output"`
	Error      string `json:"error"`
	OutputFile string `json:"output_file,omitempty"`
}

// invocation is the per-run_code context. It owns the channel map for
// in-flight inner tool calls and the fresh Yaegi exports built from the
// current tool registry.
type invocation struct {
	ctx    context.Context
	bus    engine.EventBus
	logger interface{ Info(string, ...any) }
	turnID string

	mu      sync.Mutex
	pending map[string]chan events.ToolResult // keyed by ToolCall.ID
}

func newInvocation(ctx context.Context, bus engine.EventBus, turnID string) *invocation {
	return &invocation{
		ctx:     ctx,
		bus:     bus,
		turnID:  turnID,
		pending: make(map[string]chan events.ToolResult),
	}
}

// routeResult delivers a ToolResult to the waiting shim, if any. Called by
// the plugin's tool.result subscription. Returns true if the result was
// routed (the caller can then stop looking further).
func (inv *invocation) routeResult(result events.ToolResult) bool {
	inv.mu.Lock()
	ch, ok := inv.pending[result.ID]
	inv.mu.Unlock()
	if !ok {
		return false
	}
	// Buffered channel of size 1 — non-blocking even if bus is synchronous
	// and the shim goroutine hasn't yet reached its receive.
	select {
	case ch <- result:
	default:
	}
	return true
}

// registerPending reserves a channel for the given call ID and returns it.
func (inv *invocation) registerPending(id string) chan events.ToolResult {
	ch := make(chan events.ToolResult, 1)
	inv.mu.Lock()
	inv.pending[id] = ch
	inv.mu.Unlock()
	return ch
}

func (inv *invocation) clearPending(id string) {
	inv.mu.Lock()
	delete(inv.pending, id)
	inv.mu.Unlock()
}

// buildToolsExports turns the current tool registry into a Yaegi Exports entry
// at import path "tools". Each tool becomes a typed function
//
//	func <ToolName>(args <ToolName>Args) (Result, error)
//
// whose implementation round-trips through the event bus (including all
// before:tool.invoke gates). The Result type is a fixed tools.Result struct
// so scripts get the same shape regardless of the underlying tool.
func (inv *invocation) buildToolsExports(tools []events.ToolDef, skipSelf string) (interp.Exports, map[string]reflect.Type, error) {
	pkg := map[string]reflect.Value{}
	argTypes := map[string]reflect.Type{}

	// tools.Result is a known type that every shim returns.
	resultType := reflect.TypeOf(toolResult{})
	pkg["Result"] = reflect.Zero(reflect.PointerTo(resultType))

	for _, td := range tools {
		if td.Name == skipSelf {
			continue
		}
		argsType, err := schemaToType(td.Parameters, toolFuncName(td.Name))
		if err != nil {
			return nil, nil, fmt.Errorf("tool %s: %w", td.Name, err)
		}
		// Force-box args as struct even if the schema produced a map, so the
		// script has something typed to construct.
		if argsType.Kind() == reflect.Map {
			argsType = reflect.StructOf(nil)
		}

		argsTypeName := toolArgsTypeName(td.Name)
		funcName := toolFuncName(td.Name)
		argTypes[td.Name] = argsType

		pkg[argsTypeName] = reflect.Zero(reflect.PointerTo(argsType))

		funcType := reflect.FuncOf(
			[]reflect.Type{argsType},
			[]reflect.Type{resultType, errorInterface},
			false,
		)
		toolName := td.Name // capture
		at := argsType
		fn := reflect.MakeFunc(funcType, func(in []reflect.Value) []reflect.Value {
			result, err := inv.callTool(toolName, in[0], at)
			return []reflect.Value{
				reflect.ValueOf(result),
				errorValue(err),
			}
		})
		pkg[funcName] = fn
	}

	return interp.Exports{"tools/tools": pkg}, argTypes, nil
}

// callTool is the actual bus round-trip invoked by a shim function. It
// marshals the typed args into a map[string]any, emits before:tool.invoke
// + tool.invoke, and blocks until the matching tool.result arrives (or
// the invocation context is cancelled).
func (inv *invocation) callTool(name string, argsVal reflect.Value, argsType reflect.Type) (toolResult, error) {
	// Convert the typed args struct into map[string]any by round-tripping
	// through JSON. Tool plugins consume Arguments as map[string]any.
	raw, err := marshalArgs(argsVal, argsType)
	if err != nil {
		return toolResult{}, fmt.Errorf("marshal args for %s: %w", name, err)
	}

	callID := "code-" + randID()
	tc := events.ToolCall{
		ID:        callID,
		Name:      name,
		Arguments: raw,
		TurnID:    inv.turnID,
	}

	ch := inv.registerPending(callID)
	defer inv.clearPending(callID)

	// Vetoable gate pass.
	veto, err := inv.bus.EmitVetoable("before:tool.invoke", &tc)
	if err != nil {
		return toolResult{}, fmt.Errorf("emit before:tool.invoke: %w", err)
	}
	if veto.Vetoed {
		return toolResult{Error: veto.Reason}, fmt.Errorf("tool %s vetoed: %s", name, veto.Reason)
	}

	if err := inv.bus.Emit("tool.invoke", tc); err != nil {
		return toolResult{}, fmt.Errorf("emit tool.invoke: %w", err)
	}

	// Wait for the matching tool.result.
	select {
	case res := <-ch:
		out := toolResult{
			Output:     res.Output,
			Error:      res.Error,
			OutputFile: res.OutputFile,
		}
		if res.Error != "" {
			return out, fmt.Errorf("tool %s failed: %s", name, res.Error)
		}
		return out, nil
	case <-inv.ctx.Done():
		return toolResult{}, inv.ctx.Err()
	}
}

func marshalArgs(val reflect.Value, typ reflect.Type) (map[string]any, error) {
	// Ensure we have an addressable/interface-able value of the right type.
	if !val.IsValid() {
		return map[string]any{}, nil
	}
	if val.Type() != typ {
		if val.Type().ConvertibleTo(typ) {
			val = val.Convert(typ)
		}
	}
	b, err := json.Marshal(val.Interface())
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		// Args was a non-object; wrap it.
		out = map[string]any{"value": nil}
		if err2 := json.Unmarshal(b, &out); err2 != nil {
			return nil, err
		}
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// errorInterface is the reflect.Type of the error interface — preallocated
// because reflect.FuncOf signatures need a stable reference.
var errorInterface = reflect.TypeOf((*error)(nil)).Elem()

func errorValue(err error) reflect.Value {
	if err == nil {
		return reflect.Zero(errorInterface)
	}
	return reflect.ValueOf(err)
}

var callCounter uint64

// randID returns a short, collision-resistant identifier for inner tool calls.
// Format: "<seq>-<8 hex chars of crypto/rand>".
func randID() string {
	n := atomic.AddUint64(&callCounter, 1)
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("%d-%s", n, hex.EncodeToString(buf[:]))
}
