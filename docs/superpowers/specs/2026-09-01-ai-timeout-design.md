# AI Timeout Unification Design

## Goal

Give every backend LLM operation up to 10 minutes and ensure no shorter outer context or lock cancels it first. Ask models to respond quickly, with problem classification favoring approximate useful results over prolonged reasoning.

## Scope

- Define one shared 10-minute LLM call timeout in the common OpenAI client package.
- Use it for the problem analyzer and agent chat HTTP clients.
- Give the problem-analysis consumer, AI daily report, weekly report, and training-report task a 10-minute execution context.
- Set duplicate-prevention lock TTLs covering those 10-minute operations.
- Leave non-AI data loading, Redis writes, email delivery, crawler operations, and frontend HTTP defaults unchanged.

## Prompt Behavior

- Problem analysis must prioritize speed and avoid lengthy reasoning.
- Problem type, difficulty, tags, and suggested solutions may be approximate based on the statement and do not need absolute precision.
- The complete Chinese statement translation remains mandatory and must not be shortened for speed.
- Daily and training-report prompts must request a quick, direct response without lengthy reasoning.

## Error Handling

- Existing problem-analysis retry behavior remains unchanged: one failed attempt updates the problem failure state and RabbitMQ retries up to the current limit.
- Daily and training reports retain their existing rule-template fallback behavior when AI fails.
- Timeout errors continue through existing status and logging paths.

## Verification

- Unit tests assert the shared timeout equals 10 minutes.
- Prompt tests assert the speed-first and approximate-classification requirements.
- Existing agent and core-data service tests must continue to pass.
