package handlers

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/context"
)

type modelRewriteAppliedContextKey struct{}

// modelRewriteObserver is a test hook called once for every applied rewrite.
var modelRewriteObserver func(from, to string)

// RewriteModelName returns the model name after routing.model-rewrite, keeping any
// thinking suffix. It is a pure single pass: it neither logs nor marks a request,
// so lookups (usage, quota summaries) can mirror what execution will use.
func (h *BaseAPIHandler) RewriteModelName(modelName string) string {
	if h == nil || h.Cfg == nil {
		return modelName
	}
	rewritten, _ := rewriteModelName(h.Cfg.ModelRewrite, modelName)
	return rewritten
}

// rewriteModelName rewrites the base model name with the first matching rule and
// re-appends the request's thinking suffix. The result is never fed back into the rules.
func rewriteModelName(rules []config.ModelRewriteRule, modelName string) (string, bool) {
	if len(rules) == 0 {
		return modelName, false
	}
	parsed := thinking.ParseSuffix(strings.TrimSpace(modelName))
	base := strings.TrimSpace(parsed.ModelName)
	target, ok := config.MatchModelRewrite(rules, base)
	if !ok || strings.EqualFold(base, target) {
		return modelName, false
	}
	if parsed.HasSuffix {
		target = strings.TrimSpace(thinking.ParseSuffix(target).ModelName) + "(" + parsed.RawSuffix + ")"
	}
	if target == modelName {
		return modelName, false
	}
	return target, true
}

func modelRewriteAppliedFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	applied, _ := ctx.Value(modelRewriteAppliedContextKey{}).(bool)
	return applied
}

// applyModelRewrite applies routing.model-rewrite once per request, at execution
// entry and before model routing, provider resolution and metadata capture. The
// returned context is marked so a re-entered execution path for the same request
// skips the rules; internal host-model callbacks are never rewritten. When the body
// carries the same model, its "model" field is rewritten too so translators and
// executors that read the payload see the target.
func (h *BaseAPIHandler) applyModelRewrite(ctx context.Context, modelName string, rawJSON []byte, execOptions modelExecutionOptions) (context.Context, string, []byte) {
	if ctx == nil {
		ctx = context.Background()
	}
	if execOptions.InternalSource || modelRewriteAppliedFromContext(ctx) || h == nil || h.Cfg == nil || len(h.Cfg.ModelRewrite) == 0 {
		return ctx, modelName, rawJSON
	}
	ctx = context.WithValue(ctx, modelRewriteAppliedContextKey{}, true)
	rewritten, ok := rewriteModelName(h.Cfg.ModelRewrite, modelName)
	if !ok {
		return ctx, modelName, rawJSON
	}
	if bodyModel := gjson.GetBytes(rawJSON, "model"); bodyModel.Type == gjson.String && strings.TrimSpace(bodyModel.String()) == strings.TrimSpace(modelName) {
		if updated, errSet := sjson.SetBytes(rawJSON, "model", rewritten); errSet == nil {
			rawJSON = updated
		} else {
			log.WithError(errSet).Warn("model rewrite: failed to update request body model")
		}
	}
	log.WithFields(log.Fields{
		"request_id": logging.GetRequestID(ctx),
		"from":       modelName,
		"to":         rewritten,
	}).Debug("model rewrite applied")
	if modelRewriteObserver != nil {
		modelRewriteObserver(modelName, rewritten)
	}
	return ctx, rewritten, rawJSON
}
