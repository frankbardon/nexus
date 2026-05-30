package builtins

import (
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
)

// NativeRegistrar is the minimal surface RegisterAll needs from a host.
// *predicates.Evaluator satisfies it; tests can substitute fakes.
type NativeRegistrar interface {
	RegisterNative(name string, h predicates.NativeHandler)
}

// RegisterAll registers every baked-in native predicate handler with
// reg under its canonical name. The current Evaluator.RegisterNative
// signature has no failure mode, so RegisterAll always returns nil; the
// error return is reserved for future additions (e.g. duplicate-name
// detection, schema preflight) without a signature change.
func RegisterAll(reg NativeRegistrar) error {
	reg.RegisterNative(HandlerWordCountUnder, WordCountUnder{})
	reg.RegisterNative(HandlerWordCountOver, WordCountOver{})
	reg.RegisterNative(HandlerContainsRequiredIDs, ContainsRequiredIDs{})
	reg.RegisterNative(HandlerJSONPathExists, JSONPathExists{})
	return nil
}
