# Queued Problem Jobs Design

## Goal

Show queued problem-fetch and AI-analysis rows alongside active worker jobs in the processing panel.

## Data And Status

- Reuse `inProgress` from the existing progress API; do not add an endpoint.
- Treat rows already present in `activeJobs` as executing and exclude their duplicates from the queued rows.
- Map remaining `FETCHING` rows to `题面获取 · 排队中`.
- Map remaining `TAGGING` rows to `AI 分析 · 排队中`.
- Do not show submission-sync queue details because RabbitMQ exposes only its count, not individual problem rows.

## UI

- Rename the panel to cover both executing and queued jobs.
- The badge count is executing plus visible queued rows.
- Active AI jobs remain selectable for live output.
- Queued rows are informational and do not open live output.

## Verification

- Unit-test active/queued deduplication and status labels.
- Run the frontend test suite and production build.
