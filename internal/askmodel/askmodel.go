// Package askmodel carries the model a request named past an API that has
// no word for it, and so is no part of anybody's API: from the program's
// `rota run --model` and the server's run to the rotation's choice, which
// reads it from the context it is given.
//
// A context without one is a request that named no model, which is every
// caller outside this module: the rotation then chooses exactly as it always
// has.
package askmodel

import "context"

type key struct{}

// With is ctx carrying the model a request named. An empty model is stored
// too, so a request that names none, made inside one that named one, does
// not inherit the outer request's model.
func With(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, key{}, model)
}

// From is the model ctx carries, or "" when the request named none.
func From(ctx context.Context) string {
	m, _ := ctx.Value(key{}).(string)
	return m
}
