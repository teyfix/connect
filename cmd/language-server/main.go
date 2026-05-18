package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/redpanda-data/benthos/v4/public/bloblang"
	_ "github.com/redpanda-data/connect/public/bundle/free/v4"
	"github.com/tliron/commonlog"
	_ "github.com/tliron/commonlog/simple"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
	"github.com/tliron/glsp/server"
)

const lsName = "bloblang"

var version = "0.0.1"
var bloblangCompletionCache []protocol.CompletionItem

// includeDeprecated controls whether deprecated functions/methods appear in
// completions. When true they are included with a visual warning prefix and the
// LSP deprecated tag. Set to true during migration work if needed.
const includeDeprecated = false

func main() {
	log.Printf("[main] Building Bloblang completion cache...")
	bloblangCompletionCache = buildBloblangCache()

	log.Printf("[main] Starting LSP server...")

	commonlog.Configure(1, nil)
	handler := protocol.Handler{
		Initialize:             initialize,
		Initialized:            initialized,
		Shutdown:               shutdown,
		SetTrace:               setTrace,
		TextDocumentDidOpen:    didOpen,
		TextDocumentDidChange:  didChange,
		TextDocumentDidClose:   didClose,
		TextDocumentCompletion: completion,
		TextDocumentHover:      hover,
	}

	s := server.NewServer(&handler, lsName, false)
	s.RunStdio()
}

func initialize(context *glsp.Context, params *protocol.InitializeParams) (any, error) {
	log.Printf("[initialize] received event")
	capabilities := handlerCapabilities()

	return protocol.InitializeResult{
		Capabilities: capabilities,
		ServerInfo: &protocol.InitializeResultServerInfo{
			Name:    lsName,
			Version: &version,
		},
	}, nil
}

func initialized(context *glsp.Context, params *protocol.InitializedParams) error {
	log.Printf("[initialized] received event")
	return nil
}

func shutdown(context *glsp.Context) error {
	log.Printf("[shutdown] received event")
	return nil
}

func setTrace(context *glsp.Context, params *protocol.SetTraceParams) error {
	log.Printf("[setTrace] received event")
	protocol.SetTraceValue(params.Value)
	return nil
}

func handlerCapabilities() protocol.ServerCapabilities {
	openClose := true
	workDoneProgress := true
	syncKind := protocol.TextDocumentSyncKindFull

	return protocol.ServerCapabilities{
		TextDocumentSync: &protocol.TextDocumentSyncOptions{
			OpenClose: &openClose,
			Change:    &syncKind,
		},
		CompletionProvider: &protocol.CompletionOptions{
			TriggerCharacters: []string{".", "@", "$"},
		},
		HoverProvider: &protocol.HoverOptions{
			WorkDoneProgressOptions: protocol.WorkDoneProgressOptions{
				WorkDoneProgress: &workDoneProgress,
			},
		},
	}
}

// ─── Status helpers ───────────────────────────────────────────────────────────

// statusPrefix returns a visual label prefix for non-stable statuses so they
// stand out in the completion picker.
func statusPrefix(status string) string {
	switch status {
	case "beta":
		return "[β] "
	case "experimental":
		return "[⚗] "
	case "deprecated":
		return "[⚠] "
	default:
		return ""
	}
}

// statusSortText returns a sort key prefix that pushes unstable items below
// stable ones in the picker (stable → beta/experimental → deprecated).
func statusSortText(status, name string) string {
	switch status {
	case "stable", "":
		return "0_" + name
	case "beta", "experimental":
		return "1_" + name
	default:
		return "2_" + name
	}
}

// ─── Param introspection ──────────────────────────────────────────────────────

// isOptionalParam returns true when a param has a marshalled default value or
// is explicitly flagged as optional.
func isOptionalParam(p bloblang.TemplateParamData) bool {
	return p.IsOptional || p.DefaultMarshalled != ""
}

// hasOptionalParams returns true when at least one param in the set is optional.
func hasOptionalParams(params bloblang.TemplateParamsData) bool {
	for _, p := range params.Definitions {
		if isOptionalParam(p) {
			return true
		}
	}
	return false
}

// inferSnippetPlaceholder returns the best tab-stop placeholder text for a
// param, preferring the actual marshalled default value over a type-based
// fallback so the user sees real values (e.g. "1", "9223372036854775807")
// rather than the generic "null".
func inferSnippetPlaceholder(p bloblang.TemplateParamData) string {
	if p.DefaultMarshalled != "" {
		return p.DefaultMarshalled
	}
	switch strings.ToLower(p.ValueType) {
	case "string":
		return ""
	case "integer", "number", "float":
		return "0"
	case "bool":
		return "true"
	case "array":
		return "[]"
	case "object":
		return "{}"
	case "query expression":
		// Use the param name as a readable hint inside the tab stop.
		return p.Name
	default:
		return "null"
	}
}

// ─── Snippet generation ───────────────────────────────────────────────────────

// generatePositionalSnippet produces:
//   - snippet: tab-stop string for REQUIRED params only (positional style).
//     Optional params are intentionally omitted so the base completion stays
//     minimal and uncluttered.
//   - signature: full human-readable signature for ALL params (optional ones
//     marked with "?") used in the detail line.
func generatePositionalSnippet(params bloblang.TemplateParamsData) (snippet, signature string) {
	var snippetParts []string
	var sigParts []string

	tabIndex := 1
	for _, p := range params.Definitions {
		optional := isOptionalParam(p)
		optMark := ""
		if optional {
			optMark = "?"
		}
		sigParts = append(sigParts, fmt.Sprintf("%s%s: %s", p.Name, optMark, p.ValueType))

		// Positional snippet only includes required params; the named-args
		// variant (generated separately) covers optional ones.
		if optional {
			continue
		}

		pType := strings.ToLower(p.ValueType)
		placeholder := inferSnippetPlaceholder(p)

		switch pType {
		case "query expression":
			// Lambda-style: all(test -> ${1:test})
			snippetParts = append(snippetParts, fmt.Sprintf("%s -> ${%d:%s}", p.Name, tabIndex, p.Name))
		case "string":
			snippetParts = append(snippetParts, fmt.Sprintf("\"${%d:%s}\"", tabIndex, placeholder))
		default:
			snippetParts = append(snippetParts, fmt.Sprintf("${%d:%s}", tabIndex, placeholder))
		}
		tabIndex++
	}

	if len(snippetParts) == 0 && params.Variadic {
		snippetParts = append(snippetParts, "\"${1:value}\"")
		sigParts = append(sigParts, "...params")
	}

	return strings.Join(snippetParts, ", "), strings.Join(sigParts, ", ")
}

// generateNamedArgsSnippet produces a named-argument snippet that includes ALL
// params (required + optional). For query-expression params that have a default
// (i.e. they accept scalars via ScalarsToLiteral), it uses name: value syntax.
// For pure lambda params (no default), it uses the arrow syntax as a hint.
//
// Example output: "min: ${1:1}, max: ${2:9223372036854775807}, set: ${3:null}"
func generateNamedArgsSnippet(params bloblang.TemplateParamsData) string {
	var parts []string
	for i, p := range params.Definitions {
		tabIndex := i + 1
		pType := strings.ToLower(p.ValueType)
		placeholder := inferSnippetPlaceholder(p)

		var part string
		switch pType {
		case "string":
			part = fmt.Sprintf("%s: \"${%d:%s}\"", p.Name, tabIndex, placeholder)
		case "query expression":
			if p.DefaultMarshalled != "" {
				// Has a default → likely a ScalarsToLiteral param; use named scalar syntax.
				part = fmt.Sprintf("%s: ${%d:%s}", p.Name, tabIndex, placeholder)
			} else {
				// No default → lambda param; use arrow syntax as a hint.
				part = fmt.Sprintf("%s: %s -> ${%d:%s}", p.Name, p.Name, tabIndex, p.Name)
			}
		default:
			part = fmt.Sprintf("%s: ${%d:%s}", p.Name, tabIndex, placeholder)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// ─── Documentation builder ───────────────────────────────────────────────────
func renderCodeBlock(value string) string {
	value = strings.TrimSpace(value)

	var parsed any

	// Try JSON parse first
	if err := json.Unmarshal([]byte(value), &parsed); err == nil {
		pretty, err := json.MarshalIndent(parsed, "", "  ")
		if err == nil {
			return "```json\n" + string(pretty) + "\n```\n"
		}
	}

	// Fallback
	return "```txt\n" + value + "\n```\n"
}

// buildDocumentation assembles a Markdown MarkupContent value from all
// available metadata: status badge, description, examples (with input/output
// tables), method categories, and version footer.
func buildDocumentation(
	description string,
	examples []bloblang.TemplateExampleData,
	categories []bloblang.TemplateMethodCategoryData,
	ver string,
	status string,
) protocol.MarkupContent {
	var sb strings.Builder

	// Status badge — shown at the top so it's immediately visible on hover.
	switch status {
	case "beta":
		sb.WriteString("> **[β Beta]** This component is in beta and its behaviour may change.\n\n")
	case "experimental":
		sb.WriteString("> **[⚗ Experimental]** This component is experimental and may be removed or changed.\n\n")
	case "deprecated":
		sb.WriteString("> **[⚠ Deprecated]** Avoid using this in new mappings.\n\n")
	}

	if description != "" {
		sb.WriteString(description)
		sb.WriteString("\n\n")
	}

	// Examples section — capped at 3 to keep hover docs readable.
	const maxExamples = 3
	if len(examples) > 0 {
		sb.WriteString("**Examples**\n\n")

		shown := examples
		truncated := 0
		if len(examples) > maxExamples {
			shown = examples[:maxExamples]
			truncated = len(examples) - maxExamples
		}

		for i, ex := range shown {
			if ex.Summary != "" {
				sb.WriteString("_" + ex.Summary + "_\n\n")
			}

			sb.WriteString("### Mapping\n\n")
			sb.WriteString("```bloblang\n")
			sb.WriteString(strings.TrimSpace(ex.Mapping))
			sb.WriteString("\n```\n\n")

			// Render the input→output table only when the example is tested
			// (SkipTesting examples may produce different results in practice).
			if len(ex.Results) > 0 && !ex.SkipTesting {
				for j, r := range ex.Results {
					// Escape pipe characters so the Markdown table stays valid.

					suffix := ""

					if len(shown) > 1 {
						suffix = fmt.Sprintf(" — Example #%d", j+1)
					}

					sb.WriteString(
						fmt.Sprintf("#### Input%s\n\n", suffix),
					)
					sb.WriteString(renderCodeBlock(r[0]))
					sb.WriteString("\n")

					sb.WriteString(
						fmt.Sprintf("#### Output%s\n\n", suffix),
					)
					sb.WriteString(renderCodeBlock(r[1]))
					sb.WriteString("\n")
				}
				sb.WriteString("\n")
			}

			if ex.SkipTesting {
				sb.WriteString("> ⚠ This example is not tested in CI — results may vary.\n\n")
			}

			// Separator between examples
			if i < len(shown)-1 {
				sb.WriteString("---\n\n")
			}
		}

		if truncated > 0 {
			sb.WriteString(fmt.Sprintf("_…and %d more examples_\n\n", truncated))
		}
	}

	// Method categories — only present for MethodView items.
	if len(categories) > 0 {
		var catNames []string
		for _, c := range categories {
			catNames = append(catNames, c.Category)
		}
		sb.WriteString("**Categories:** " + strings.Join(catNames, ", ") + "\n\n")

		// If a category carries its own description, surface it as a sub-note.
		for _, c := range categories {
			if c.Description != "" {
				sb.WriteString(fmt.Sprintf("_%s_: %s\n\n", c.Category, c.Description))
			}
		}
	}

	// Version footer.
	if ver != "" {
		sb.WriteString("---\n")
		sb.WriteString(fmt.Sprintf("_Introduced in Benthos **%s**_\n", ver))
	}

	return protocol.MarkupContent{
		Kind:  protocol.MarkupKindMarkdown,
		Value: strings.TrimSpace(sb.String()),
	}
}

// ─── Completion item builders ─────────────────────────────────────────────────

// buildFunctionItem creates one CompletionItem for a Bloblang function.
// When namedArgs is true the item represents the "named args" variant.
func buildFunctionItem(
	data bloblang.TemplateFunctionData,
	name, snippet, signature string,
	kind protocol.CompletionItemKind,
	namedArgs bool,
) protocol.CompletionItem {
	prefix := statusPrefix(data.Status)
	label := prefix + name
	if namedArgs {
		label += " (named args)"
	}

	insertText := name + "()"
	if snippet != "" {
		insertText = fmt.Sprintf("%s(%s)", name, snippet)
	}

	detailText := fmt.Sprintf("%s(%s)", name, signature)
	if data.Category != "" {
		detailText += fmt.Sprintf(" — Function (%s)", data.Category)
	}
	// NOTE: Impure is not forwarded through TemplateFunctionData in the public
	// bloblang API (view.go). If Redpanda exposes it in a future release, append
	// " ⚡ impure" to detailText here and add the warning block to buildDocumentation.

	sortText := statusSortText(data.Status, name)
	if namedArgs {
		sortText += "_z" // push named-args variant just below the base item
	}

	doc := buildDocumentation(data.Description, data.Examples, nil, data.Version, data.Status)

	item := protocol.CompletionItem{
		Label:            label,
		Kind:             &kind,
		Detail:           &detailText,
		Documentation:    doc,
		InsertText:       &insertText,
		InsertTextFormat: ptrTo(protocol.InsertTextFormatSnippet),
		SortText:         &sortText,
	}

	if data.Status == "deprecated" {
		item.Tags = []protocol.CompletionItemTag{protocol.CompletionItemTagDeprecated}
	}

	return item
}

// buildMethodItem creates one CompletionItem for a Bloblang method.
// When namedArgs is true the item represents the "named args" variant.
func buildMethodItem(
	data bloblang.TemplateMethodData,
	name, snippet, signature string,
	kind protocol.CompletionItemKind,
	namedArgs bool,
) protocol.CompletionItem {
	prefix := statusPrefix(data.Status)
	label := prefix + name
	if namedArgs {
		label += " (named args)"
	}

	insertText := name + "()"
	if snippet != "" {
		insertText = fmt.Sprintf("%s(%s)", name, snippet)
	}

	// Use the first category (if any) in the detail line for quick context.
	primaryCat := ""
	if len(data.Categories) > 0 {
		primaryCat = data.Categories[0].Category
	}
	detailText := fmt.Sprintf(".%s(%s) — Method", name, signature)
	if primaryCat != "" {
		detailText += fmt.Sprintf(" · %s", primaryCat)
	}

	sortText := statusSortText(data.Status, name)
	if namedArgs {
		sortText += "_z"
	}

	doc := buildDocumentation(data.Description, data.Examples, data.Categories, data.Version, data.Status)

	item := protocol.CompletionItem{
		Label:            label,
		Kind:             &kind,
		Detail:           &detailText,
		Documentation:    doc,
		InsertText:       &insertText,
		InsertTextFormat: ptrTo(protocol.InsertTextFormatSnippet),
		SortText:         &sortText,
	}

	if data.Status == "deprecated" {
		item.Tags = []protocol.CompletionItemTag{protocol.CompletionItemTagDeprecated}
	}

	return item
}

// ─── Cache builder ────────────────────────────────────────────────────────────

func buildBloblangCache() []protocol.CompletionItem {
	var items []protocol.CompletionItem
	env := bloblang.GlobalEnvironment()

	kindFunc := protocol.CompletionItemKindFunction
	kindMethod := protocol.CompletionItemKindMethod

	// Functions
	env.WalkFunctions(func(name string, view *bloblang.FunctionView) {
		data := view.TemplateData()

		switch data.Status {
		case "hidden":
			return // never surface hidden items
		case "deprecated":
			if !includeDeprecated {
				return
			}
		}

		snippet, signature := generatePositionalSnippet(data.Params)
		items = append(items, buildFunctionItem(data, name, snippet, signature, kindFunc, false))

		// When optional params exist, also offer a named-args variant so users
		// can selectively supply middle optional arguments by name.
		if hasOptionalParams(data.Params) {
			namedSnippet := generateNamedArgsSnippet(data.Params)
			items = append(items, buildFunctionItem(data, name, namedSnippet, signature, kindFunc, true))
		}
	})

	// Methods
	env.WalkMethods(func(name string, view *bloblang.MethodView) {
		data := view.TemplateData()

		switch data.Status {
		case "hidden":
			return
		case "deprecated":
			if !includeDeprecated {
				return
			}
		}

		snippet, signature := generatePositionalSnippet(data.Params)
		items = append(items, buildMethodItem(data, name, snippet, signature, kindMethod, false))

		if hasOptionalParams(data.Params) {
			namedSnippet := generateNamedArgsSnippet(data.Params)
			items = append(items, buildMethodItem(data, name, namedSnippet, signature, kindMethod, true))
		}
	})

	return items
}

// ─── LSP handlers ─────────────────────────────────────────────────────────────

func completion(context *glsp.Context, params *protocol.CompletionParams) (any, error) {
	log.Printf("[completion] received event for %s", params.TextDocument.URI)

	if bloblangCompletionCache == nil {
		log.Println("[completion] Populating Bloblang completion cache...")
		bloblangCompletionCache = buildBloblangCache()
	}

	return bloblangCompletionCache, nil
}

func didOpen(ctx *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	uri := params.TextDocument.URI
	text := params.TextDocument.Text

	log.Printf("[didOpen] received event: %s", uri)
	log.Printf("[didOpen] extracted content: %d bytes", len(text))

	validateMapping(ctx, uri, text)

	return nil
}

func hover(context *glsp.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
	log.Printf(
		"[hover] line=%d char=%d",
		params.Position.Line,
		params.Position.Character,
	)

	if params.WorkDoneToken != nil {
		log.Printf("[hover:WorkDoneToken] value=%s",
			params.WorkDoneToken.Value,
		)
	} else {
		log.Printf("[hover:WorkDoneToken] nil")
	}

	if params.WorkDoneProgressParams.WorkDoneToken != nil {
		log.Printf("[hover:WorkDoneProgressParams:WorkDoneToken] value=%s",
			params.WorkDoneProgressParams.WorkDoneToken.Value,
		)
	} else {
		log.Printf("[hover:WorkDoneProgressParams:WorkDoneToken] nil")

	}

	return nil, nil
}

func didChange(ctx *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
	uri := params.TextDocument.URI

	log.Printf("[didChange] received event: %s", uri)

	if len(params.ContentChanges) == 0 {
		log.Printf("[didChange] no changes made")
		return nil
	}

	var text string
	switch change := params.ContentChanges[0].(type) {
	case protocol.TextDocumentContentChangeEvent:
		text = change.Text
	case protocol.TextDocumentContentChangeEventWhole:
		text = change.Text
	}

	log.Printf("[didChange] extracted content: %d bytes", len(text))

	validateMapping(ctx, uri, text)

	return nil
}

func didClose(ctx *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	log.Printf("[didClose] received event: %s", params.TextDocument.URI)
	return nil
}

// ─── Validation ───────────────────────────────────────────────────────────────

func uriToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}

	if u.Scheme != "file" {
		return "", fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}

	p := u.Path

	// Windows: strip leading slash from /C:/...
	if os.PathSeparator == '\\' && len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}

	return filepath.FromSlash(p), nil
}

func customImporter(uri string, name string) ([]byte, error) {
	basePath, err := uriToPath(uri)
	if err != nil {
		return nil, err
	}

	return os.ReadFile(filepath.Join(path.Dir(basePath), name))
}

func validateMapping(ctx *glsp.Context, uri string, text string) {
	log.Printf("[validateMapping] starting validation for %s", uri)

	if text == "" {
		log.Printf("[validateMapping] empty content, clearing diagnostics")
		publishDiagnostics(ctx, uri, nil)
		return
	}

	log.Printf("[validateMapping] initializing environment")
	env := bloblang.GlobalEnvironment().WithCustomImporter(func(name string) ([]byte, error) {
		return customImporter(uri, name)
	})

	log.Printf("[validateMapping] parsing mapping")
	_, err := env.Parse(text)

	if err == nil {
		log.Printf("[validateMapping] no errors found")
		publishDiagnostics(ctx, uri, nil)
		return
	}

	log.Printf("[validateMapping] error found: %v", err)
	log.Printf("[validateMapping] converting error to diagnostics")
	diagnostics := convertErrorToDiagnostics(text, err)

	publishDiagnostics(ctx, uri, diagnostics)
}

func convertErrorToDiagnostics(text string, err error) []protocol.Diagnostic {
	severity := protocol.DiagnosticSeverityError
	source := "bloblang"

	rawErr := ""
	prettyErr, ok := err.(*bloblang.ParseError)
	if ok {
		rawErr = prettyErr.ErrorMultiline()
	} else {
		rawErr = err.Error()
	}

	log.Printf("[convertError] raw error: %s", rawErr)

	var line, char int
	idx := strings.Index(rawErr, "line ")
	if idx == -1 {
		log.Printf("[convertError] could not find line/char pattern in error, using fallback")
		return []protocol.Diagnostic{
			{
				Range:    protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 0}},
				Severity: &severity,
				Message:  rawErr,
				Source:   &source,
			},
		}
	}

	_, err = fmt.Sscanf(rawErr[idx:], "line %d char %d", &line, &char)
	if err != nil {
		log.Printf("[convertError] failed to parse line/char from error: %v", err)
		return []protocol.Diagnostic{
			{
				Range:    protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 0}},
				Severity: &severity,
				Message:  err.Error(),
				Source:   &source,
			},
		}
	}

	cleanMessage := rawErr
	if parts := strings.SplitN(rawErr, ": ", 2); len(parts) == 2 {
		cleanMessage = parts[1]
	}
	cleanMessage = strings.Split(cleanMessage, "\n")[0]

	if line > 0 {
		line--
	}
	if char > 0 {
		char--
	}

	return []protocol.Diagnostic{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: protocol.UInteger(line), Character: protocol.UInteger(char)},
				End:   protocol.Position{Line: protocol.UInteger(line), Character: protocol.UInteger(char)},
			},
			Severity: &severity,
			Message:  cleanMessage,
			Source:   &source,
		},
	}
}

func publishDiagnostics(ctx *glsp.Context, uri string, diagnostics []protocol.Diagnostic) {
	if diagnostics == nil {
		diagnostics = []protocol.Diagnostic{}
	}

	log.Printf("[publishDiagnostics] publishing %d diagnostics for %s", len(diagnostics), uri)
	ctx.Notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: diagnostics,
	})
}

// ─── Utilities ────────────────────────────────────────────────────────────────

// ptrTo returns a pointer to v. Useful for taking the address of literals and
// local values when the protocol structs require pointer fields.
func ptrTo[T any](v T) *T {
	return &v
}
