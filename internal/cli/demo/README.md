# Example jobs

Seven jobs and one chain, each showing one idea. They are ordinary files: read
them, change them, break them. Nothing here is special to the engine.

  demo-hello    the smallest job that exists
  demo-counter  a durable cursor, advanced on success
  demo-flaky    the same, but failing -- watch the cursor not move
  demo-tick     a schedule, so the engine has something to do

  demo-ingest   emits an event of its own when it finishes
  demo-report   runs because that event happened, not because a clock said so
  demo-archive  runs after the report succeeds, and not at all if it does not

  chains/demo-pipeline.yaml   the only place that wiring is written down

Remove them all with: je demo --remove
