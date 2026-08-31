# AI Reasoning Stream Design

## Goal

Expose the model-provided raw reasoning stream to authorized problem-operations administrators, while keeping the final JSON output separately parseable and fixing task timestamps.

## Backend

- Keep the OpenAI-compatible Chat Completions streaming endpoint.
- Read final answer text from `choices[].delta.content` as today.
- Read raw reasoning from compatible delta fields `reasoning_content`, `reasoning`, or `reasoning_text` through the SDK's raw JSON because openai-go does not model these extension fields.
- Do not summarize, redact, synthesize, or otherwise transform reasoning text.
- Persist reasoning and final content separately in the existing Redis task conversation record.
- Store the complete system and user prompts in that record.
- Retain completed task data for one hour under the existing retention policy.

## API And UI

- Add `reasoningOutput` to the problem progress task contract.
- Show three distinct panels: raw reasoning, final output, and complete prompt.
- If no compatible reasoning field is returned, state that the current model returned no reasoning process.
- Keep access restricted by the existing problem-operations endpoint authorization.
- Parse task timestamps as Unix seconds, Unix milliseconds, or ISO/RFC3339 strings.

## Error Handling

- Unknown or malformed reasoning extension values are ignored without breaking final content streaming.
- Final JSON parsing and existing retry behavior remain unchanged.
- Reasoning may be empty even when final output succeeds.

## Verification

- Unit-test compatible reasoning delta extraction.
- Unit-test Redis persistence of reasoning, final content, complete prompt, and one-hour completion TTL.
- Unit-test ISO/RFC3339 timestamp formatting.
- Build the frontend and run backend target tests.
- Request the real authenticated progress API when credentials are available; otherwise verify its unauthenticated 401 boundary and report the authenticated test limitation.
