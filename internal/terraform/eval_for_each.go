// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/instances"
	"github.com/hashicorp/terraform/internal/lang"
	"github.com/hashicorp/terraform/internal/lang/marks"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

// evaluateForEachExpression differs from evaluateForEachExpressionValue by
// returning an error if the count value is not known, and converting the
// cty.Value to a map[string]cty.Value for compatibility with other calls.
func evaluateForEachExpression(expr hcl.Expression, ctx EvalContext) (forEach map[string]cty.Value, diags tfdiags.Diagnostics) {
	return newForEachEvaluator(expr, ctx).ResourceValue()
}

// forEachEvaluator is the standard mechanism for interpreting an expression
// given for a "for_each" argument on a resource, module, or import.
func newForEachEvaluator(expr hcl.Expression, ctx EvalContext) *forEachEvaluator {
	if ctx == nil {
		panic("nil EvalContext")
	}

	return &forEachEvaluator{
		ctx:  ctx,
		expr: expr,
	}
}

// forEachEvaluator is responsible for evaluating for_each expressions, using
// different rules depending on the desired context.
type forEachEvaluator struct {
	// We bundle this functionality into a structure, because internal
	// validation requires not only the resulting value, but also the original
	// expression and the hcl EvalContext to build the corresponding
	// diagnostic. Every method's dependency on all the evaluation pieces
	// otherwise prevents refactoring and we end up with a single giant
	// function.
	ctx  EvalContext
	expr hcl.Expression

	// internal
	hclCtx *hcl.EvalContext
}

// ResourceForEachValue returns a known for_each map[string]cty.Value
// appropriate for use within resource expansion.
func (ev *forEachEvaluator) ResourceValue() (map[string]cty.Value, tfdiags.Diagnostics) {
	res := map[string]cty.Value{}

	// no expression always results in an empty map
	if ev.expr == nil {
		return res, nil
	}

	forEachVal, diags := ev.Value()
	if diags.HasErrors() {
		return res, diags
	}

	// ensure our value is known for use in resource expansion
	diags = diags.Append(ev.ensureKnownForResource(forEachVal))
	if diags.HasErrors() {
		return res, diags
	}

	// validate the for_each value for use in resource expansion
	diags = diags.Append(ev.validateResource(forEachVal))
	if diags.HasErrors() {
		return res, diags
	}

	if forEachVal.IsNull() || !forEachVal.IsKnown() || markSafeLengthInt(forEachVal) == 0 {
		// we check length, because an empty set returns a nil map which will panic below
		return res, diags
	}

	res = forEachVal.AsValueMap()
	return res, diags
}

// ImportValue returns the for_each map for use within an import block,
// enumerated as individual instances.RepetitionData values.
func (ev *forEachEvaluator) ImportValues() ([]instances.RepetitionData, tfdiags.Diagnostics) {
	var res []instances.RepetitionData
	if ev.expr == nil {
		return res, nil
	}

	forEachVal, diags := ev.Value()
	if diags.HasErrors() {
		return res, diags
	}

	// ensure our value is known for use in resource expansion
	diags = diags.Append(ev.ensureKnownForImport(forEachVal))
	if diags.HasErrors() {
		return res, diags
	}

	if forEachVal.IsNull() {
		return res, diags
	}

	val, marks := forEachVal.Unmark()

	it := val.ElementIterator()
	for it.Next() {
		k, v := it.Element()
		res = append(res, instances.RepetitionData{
			EachKey:   k,
			EachValue: v.WithMarks(marks),
		})

	}

	return res, diags
}

// Value returns the raw cty.Value evaluated from the given for_each expression
func (ev *forEachEvaluator) Value() (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	if ev.expr == nil {
		// a nil expression always results in a null value
		return cty.NullVal(cty.Map(cty.DynamicPseudoType)), nil
	}

	refs, moreDiags := lang.ReferencesInExpr(addrs.ParseRef, ev.expr)
	diags = diags.Append(moreDiags)
	scope := ev.ctx.EvaluationScope(nil, nil, EvalDataForNoInstanceKey)
	if scope != nil {
		ev.hclCtx, moreDiags = scope.EvalContext(refs)
	} else {
		// This shouldn't happen in real code, but it can unfortunately arise
		// in unit tests due to incompletely-implemented mocks. :(
		ev.hclCtx = &hcl.EvalContext{}
	}

	diags = diags.Append(moreDiags)
	if diags.HasErrors() { // Can't continue if we don't even have a valid scope
		return cty.DynamicVal, diags
	}

	forEachVal, forEachDiags := ev.expr.Value(ev.hclCtx)
	diags = diags.Append(forEachDiags)

	return forEachVal, diags
}

// ensureKnownForImport checks that the value is entirely known for use within
// import expansion.
func (ev *forEachEvaluator) ensureKnownForImport(forEachVal cty.Value) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	if !forEachVal.IsWhollyKnown() {
		diags = diags.Append(&hcl.Diagnostic{
			Severity:    hcl.DiagError,
			Summary:     "Invalid for_each argument",
			Detail:      "The \"for_each\" expression includes values derived from other resource attributes that cannot be determined until apply, and so Terraform cannot determine the full set of values that might be used to import this resource.",
			Subject:     ev.expr.Range().Ptr(),
			Expression:  ev.expr,
			EvalContext: ev.hclCtx,
			Extra:       diagnosticCausedByUnknown(true),
		})
	}
	return diags
}

// ensureKnownForResource checks that the value is known within the rules of
// resource and module expansion.
func (ev *forEachEvaluator) ensureKnownForResource(forEachVal cty.Value) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	ty := forEachVal.Type()
	const errInvalidUnknownDetailMap = "The \"for_each\" map includes keys derived from resource attributes that cannot be determined until apply, and so Terraform cannot determine the full set of keys that will identify the instances of this resource.\n\nWhen working with unknown values in for_each, it's better to define the map keys statically in your configuration and place apply-time results only in the map values.\n\nAlternatively, you could use the -target planning option to first apply only the resources that the for_each value depends on, and then apply a second time to fully converge."
	const errInvalidUnknownDetailSet = "The \"for_each\" set includes values derived from resource attributes that cannot be determined until apply, and so Terraform cannot determine the full set of keys that will identify the instances of this resource.\n\nWhen working with unknown values in for_each, it's better to use a map value where the keys are defined statically in your configuration and where only the values contain apply-time results.\n\nAlternatively, you could use the -target planning option to first apply only the resources that the for_each value depends on, and then apply a second time to fully converge."

	if !forEachVal.IsKnown() {
		var detailMsg string
		switch {
		case ty.IsSetType():
			detailMsg = errInvalidUnknownDetailSet
		default:
			detailMsg = errInvalidUnknownDetailMap
		}

		diags = diags.Append(&hcl.Diagnostic{
			Severity:    hcl.DiagError,
			Summary:     "Invalid for_each argument",
			Detail:      detailMsg,
			Subject:     ev.expr.Range().Ptr(),
			Expression:  ev.expr,
			EvalContext: ev.hclCtx,
			Extra:       diagnosticCausedByUnknown(true),
		})
		return diags
	}

	if ty.IsSetType() && !forEachVal.IsWhollyKnown() {
		diags = diags.Append(&hcl.Diagnostic{
			Severity:    hcl.DiagError,
			Summary:     "Invalid for_each argument",
			Detail:      errInvalidUnknownDetailSet,
			Subject:     ev.expr.Range().Ptr(),
			Expression:  ev.expr,
			EvalContext: ev.hclCtx,
			Extra:       diagnosticCausedByUnknown(true),
		})
	}
	return diags
}

// ValidateResourceValue is used from validation walks to verify the validity
// of the resource for_Each expression, while still allowing for unknown
// values.
func (ev *forEachEvaluator) ValidateResourceValue() tfdiags.Diagnostics {
	val, diags := ev.Value()
	if diags.HasErrors() {
		return diags
	}

	return diags.Append(ev.validateResource(val))
}

// validateResource validates the type and values of the forEachVal, while
// still allowing unknown values for use within the validation walk.
func (ev *forEachEvaluator) validateResource(forEachVal cty.Value) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	// give an error diagnostic as this value cannot be used in for_each
	if forEachVal.HasMark(marks.Sensitive) {
		diags = diags.Append(&hcl.Diagnostic{
			Severity:    hcl.DiagError,
			Summary:     "Invalid for_each argument",
			Detail:      "Sensitive values, or values derived from sensitive values, cannot be used as for_each arguments. If used, the sensitive value could be exposed as a resource instance key.",
			Subject:     ev.expr.Range().Ptr(),
			Expression:  ev.expr,
			EvalContext: ev.hclCtx,
			Extra:       diagnosticCausedBySensitive(true),
		})
	}

	if diags.HasErrors() {
		return diags
	}
	ty := forEachVal.Type()

	switch {
	case forEachVal.IsNull():
		diags = diags.Append(&hcl.Diagnostic{
			Severity:    hcl.DiagError,
			Summary:     "Invalid for_each argument",
			Detail:      `The given "for_each" argument value is unsuitable: the given "for_each" argument value is null. A map, or set of strings is allowed.`,
			Subject:     ev.expr.Range().Ptr(),
			Expression:  ev.expr,
			EvalContext: ev.hclCtx,
		})
		return diags

	case forEachVal.Type() == cty.DynamicPseudoType:
		// We may not have any type information if this is during validation,
		// so we need to return early. During plan this can't happen because we
		// validate for unknowns first.
		return diags

	case !(ty.IsMapType() || ty.IsSetType() || ty.IsObjectType()):
		diags = diags.Append(&hcl.Diagnostic{
			Severity:    hcl.DiagError,
			Summary:     "Invalid for_each argument",
			Detail:      fmt.Sprintf(`The given "for_each" argument value is unsuitable: the "for_each" argument must be a map, or set of strings, and you have provided a value of type %s.`, ty.FriendlyName()),
			Subject:     ev.expr.Range().Ptr(),
			Expression:  ev.expr,
			EvalContext: ev.hclCtx,
		})
		return diags

	case !forEachVal.IsKnown():
		return diags

	case markSafeLengthInt(forEachVal) == 0:
		// If the map is empty ({}), return an empty map, because cty will
		// return nil when representing {} AsValueMap. This also covers an empty
		// set (toset([]))
		return diags
	}

	if ty.IsSetType() {
		// since we can't use a set values that are unknown, we treat the
		// entire set as unknown
		if !forEachVal.IsWhollyKnown() {
			return diags
		}

		if ty.ElementType() != cty.String {
			diags = diags.Append(&hcl.Diagnostic{
				Severity:    hcl.DiagError,
				Summary:     "Invalid for_each set argument",
				Detail:      fmt.Sprintf(`The given "for_each" argument value is unsuitable: "for_each" supports maps and sets of strings, but you have provided a set containing type %s.`, forEachVal.Type().ElementType().FriendlyName()),
				Subject:     ev.expr.Range().Ptr(),
				Expression:  ev.expr,
				EvalContext: ev.hclCtx,
			})
			return diags
		}

		// A set of strings may contain null, which makes it impossible to
		// convert to a map, so we must return an error
		it := forEachVal.ElementIterator()
		for it.Next() {
			item, _ := it.Element()
			if item.IsNull() {
				diags = diags.Append(&hcl.Diagnostic{
					Severity:    hcl.DiagError,
					Summary:     "Invalid for_each set argument",
					Detail:      `The given "for_each" argument value is unsuitable: "for_each" sets must not contain null values.`,
					Subject:     ev.expr.Range().Ptr(),
					Expression:  ev.expr,
					EvalContext: ev.hclCtx,
				})
				return diags
			}
		}
	}

	return diags
}

// markSafeLengthInt allows calling LengthInt on marked values safely
func markSafeLengthInt(val cty.Value) int {
	v, _ := val.UnmarkDeep()
	return v.LengthInt()
}
