# Erratum: tool results are rendered as text, not as the JSON envelope

**Relates to:** REQ-TOOL-08 (output envelope), REQ-LOOP-02, NFR-TEST-08.
**Status:** implemented 2026-09-09. Project-wide; no numbered spec.

## What the PRD says

REQ-TOOL-08 defines `ToolResult{OK, Data, Error, Detail, Terminate, Metadata}`
and says "`ToLLMMap()` strips metadata. Providers receive only the payload."
The loop read that literally: every result was `json.Marshal(ToLLMMap())`
placed in one text block, so the model saw

```
{"data":{"content":"package main\n\nimport (\n\t\"fmt\"\n)\n…","encoding":"utf-8"},"ok":true}
```

## Why it diverges

The envelope is the wrong shape for the results that dominate an agent's
context. Measured on this repository's own files, JSON-escaping a Go source
file adds 9–13% bytes; every newline, tab and quote becomes a two-byte escape
that also tokenizes as an extra token, so the estimated cost on indented code
is 15–30% more tokens per `read_file`, and the model reads code through a
layer of escaping it then has to undo when it writes an `edit_file`
`old_string`. `search_files` repeated the file path and four JSON keys on
every match (~25–40% of the result), and `execute` carried `exit_code` and
`outcome` beside `"ok":true` on every successful call.

## What changed

- `core.ToolResult.Text` is the tool's own model-facing rendering. When set,
  the loop sends it verbatim as the `tool_result` text block; `Data`, `Error`
  and `Detail` stay populated for interceptors, the audit trail and an MCP
  server bridging the tool. `ToolResult.LLMText()` is the single accessor.
- Results without `Text` keep the envelope byte for byte, so every custom
  tool and every error result is unchanged. Errors deliberately keep the
  envelope: the model keys on `error`/`detail`, and they are small.
- The built-in tools set `Text`: `read_file` (the content, with its markers),
  `execute`/`run_command`/`powershell` (the output, plus a one-line status
  only when it is informative — non-zero exit, timeout, signal, abort,
  spill path), `search_files` (grep-style, grouped by file),
  `list_files`/`find_files` (one entry per line).
- The byte and line caps of REQ-TOOL-09 apply to the same underlying content
  as before; the marker sentences of REQ-TOOL-09b/09c are unchanged.

## What did not change

`ToolResult` is still the envelope type every built-in tool returns
(REQ-TOOL-08's Go definition holds), `ToLLMMap()` still strips `Metadata` and
`Terminate`, and a consumer that inspects `ToolResultMessage.Content` for a
custom tool sees the same JSON it always did.
