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
	ctx          context.Context
	bus          engine.EventBus
	turnID       string
	parentCallID string // outer run_code tool_use_id; stamped on inner calls

	mu      sync.Mutex
	pending map[string]chan events.ToolResult // keyed by ToolCall.ID
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
// at import path "tools". Each tool becomes a typed function whose shim
// round-trips through the event bus (every before:tool.invoke gate still
// fires). Return type varies by tool:
//
//   - Tool declared an OutputSchema: func <Tool>(args) (<Tool>Result, error)
//     where <Tool>Result is a reflect.StructOf-generated struct matching the
//     schema. The shim populates fields from ToolResult.OutputStructured.
//   - Tool declared no OutputSchema: func <Tool>(args) (Result, error) —
//     the fixed tools.Result (Output/Error/OutputFile string fields).
//
// tools.Result is always exported for scripts working with schema-less tools
// or for authoring helper functions that handle both shapes.
func (inv *invocation) buildToolsExports(tools []events.ToolDef, skipSelf string) (interp.Exports, map[string]reflect.Type, error) {
	pkg := map[string]reflect.Value{}
	argTypes := map[string]reflect.Type{}

	// tools.Result is the fixed fallback result exported unconditionally.
	defaultResultType := reflect.TypeOf(toolResult{})
	pkg["Result"] = reflect.Zero(reflect.PointerTo(defaultResultType))

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

		// Pick the result type based on OutputSchema.
		resultType := defaultResultType
		typedResult := false
		if td.OutputSchema != nil {
			rt, err := schemaToType(td.OutputSchema, toolFuncName(td.Name)+"Result")
			if err != nil {
				return nil, nil, fmt.Errorf("tool %s output schema: %w", td.Name, err)
			}
			if rt.Kind() == reflect.Struct {
				resultType = rt
				typedResult = true
				pkg[toolResultTypeName(td.Name)] = reflect.Zero(reflect.PointerTo(resultType))
			}
		}

		funcType := reflect.FuncOf(
			[]reflect.Type{argsType},
			[]reflect.Type{resultType, errorInterface},
			false,
		)

		toolName := td.Name // capture
		at := argsType
		rt := resultType
		useTyped := typedResult
		fn := reflect.MakeFunc(funcType, func(in []reflect.Value) []reflect.Value {
			if useTyped {
				result, err := inv.callToolTyped(toolName, in[0], at, rt)
				return []reflect.Value{result, errorValue(err)}
			}
			result, err := inv.callTool(toolName, in[0], at)
			return []reflect.Value{reflect.ValueOf(result), errorValue(err)}
		})
		pkg[funcName] = fn
	}

	return interp.Exports{"tools/tools": pkg}, argTypes, nil
}

// callToolTyped is the shim for tools with an OutputSchema. It emits the same
// bus round-trip as callTool, but unmarshals ToolResult.OutputStructured (or
// falls back to parsing Output as JSON) into the caller-supplied resultType.
func (inv *invocation) callToolTyped(name string, argsVal reflect.Value, argsType, resultType reflect.Type) (reflect.Value, error) {
	raw, err := marshalArgs(argsVal, argsType)
	if err != nil {
		return reflect.Zero(resultType), fmt.Errorf("marshal args for %s: %w", name, err)
	}

	callID := "code-" + randID()
	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: callID,
		Name:         name,
		Arguments:    raw,
		TurnID:       inv.turnID,
		ParentCallID: inv.parentCallID,
	}

	ch := inv.registerPending(callID)
	defer inv.clearPending(callID)

	veto, err := inv.bus.EmitVetoable("before:tool.invoke", &tc)
	if err != nil {
		return reflect.Zero(resultType), fmt.Errorf("emit before:tool.invoke: %w", err)
	}
	if veto.Vetoed {
		return reflect.Zero(resultType), fmt.Errorf("tool %s vetoed: %s", name, veto.Reason)
	}

	if err := inv.bus.Emit("tool.invoke", tc); err != nil {
		return reflect.Zero(resultType), fmt.Errorf("emit tool.invoke: %w", err)
	}

	select {
	case res := <-ch:
		if res.Error != "" {
			return reflect.Zero(resultType), fmt.Errorf("tool %s failed: %s", name, res.Error)
		}
		return decodeStructuredResult(res, resultType)
	case <-inv.ctx.Done():
		return reflect.Zero(resultType), inv.ctx.Err()
	}
}

// decodeStructuredResult converts a ToolResult into a typed Go value built
// from the tool's OutputSchema. Preference order:
//
//  1. ToolResult.OutputStructured (the tool populated it directly).
//  2. ToolResult.Output parsed as a JSON object — handles existing tools
//     that already stringify JSON into Output without migrating to the new
//     field.
//  3. Output wrapped as a single "value" field when the target type can
//     accept it — lets scalar-shaped schemas still decode cleanly.
func decodeStructuredResult(res events.ToolResult, resultType reflect.Type) (reflect.Value, error) {
	target := reflect.New(resultType)

	if len(res.OutputStructured) > 0 {
		b, err := json.Marshal(res.OutputStructured)
		if err != nil {
			return reflect.Zero(resultType), fmt.Errorf("marshal structured output: %w", err)
		}
		if err := json.Unmarshal(b, target.Interface()); err != nil {
			return reflect.Zero(resultType), fmt.Errorf("unmarshal structured output: %w", err)
		}
		return target.Elem(), nil
	}

	if res.Output != "" {
		trimmed := res.Output
		if isJSONObject(trimmed) {
			if err := json.Unmarshal([]byte(trimmed), target.Interface()); err == nil {
				return target.Elem(), nil
			}
		}
	}

	// Last-ditch: return zero value. Caller sees a struct with empty fields;
	// loses nothing the script couldn't have gotten from parsing Output
	// itself.
	return target.Elem(), nil
}

// isJSONObject cheaply rejects obvious non-JSON-object payloads without
// paying for a full parse.
func isJSONObject(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return c == '{'
	}
	return false
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
	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: callID,
		Name:         name,
		Arguments:    raw,
		TurnID:       inv.turnID,
		ParentCallID: inv.parentCallID,
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
