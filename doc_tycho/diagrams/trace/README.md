# Go Execution Tracing Cheat Sheet (runtime/trace)
Simple, effective patterns for adding tracepoints that answer real questions fast.

## What tracing is good for
- “**When** does this happen?” (order/timing across goroutines)
- “**Where** do we block?” (locks, syscalls, timers, channels)
- “**How long** does each phase take?” (latency breakdown per operation/tick)
- “**Which goroutines** are involved?” (who wakes whom, and why)

---

## Minimal setup
```go
import (
  "context"
  "runtime/trace"
)

// Optional: wrap a region with a helper
func someFunction(ctx context.Context, name string) func() {
  trace.WithRegion(ctx, name, func(){})
  return func() {} // when using the block form, nothing to defer
}
```

## Core primitives to use most of the time
```go
ctx, task := trace.NewTask(context.Background(), "op-or-tick-name")
defer task.End()

trace.WithRegion(ctx, "phase-name", func() {
  // work
})

// Optional breadcrumbs
trace.Log(ctx, "key", "value")
trace.Logf(ctx, "fmt", "value=%d", 42)
```
- **Task**: a high-level operation (e.g. a "Tick", an HTTP request, a batch job)
- **Region**: a named sub-phase inside a task (parse, lock, read, compute, write)
- **Log/Logf**: tagged notes you can search in the viewer

## Tiny, reusable patters

### 1) Periodic ticker (measure wait, lock, work)
```go
go func() {
  ticker := time.NewTicker(100 * time.Millisecond)
  defer ticker.Stop()
  for {
    ctx, t := trace.NewTask(context.Background(), "tick")
    // sleep phase
    trace.WithRegion(ctx, "waitTicker", func(){ <-ticker.C })
    // lock contention + critical section
    trace.WithRegion(ctx, "Mx.Lock.acquire", func(){ mx.Lock() })
    trace.WithRegion(ctx, "UpdateCritical", func(){ doUpdate() })
    trace.WithRegion(ctx, "Mx.Unlock", func(){ mx.Unlock() })
    t.End()
  }
```
**Use it to answer**: Is the tick on schedule? how long does lock aquisition/critical work take? are we drifting?

### 2) HTTP handler latency breakdown
```go
func handler(w http.ResponseWriter, r *http.Request) {
  ctx, t := trace.NewTask(r.Context(), "http_handler")
  defer t.End()
  trace.WithRegion(ctx, "parse", func(){ parseRequest(r) })
  trace.WithRegion(ctx, "fetch", func(){ data := store.Get(ctx); _ = data })
  trace.WithRegion(ctx, "compute", func(){ compute() })
  trace.WithRegion(ctx, "encode", func(){ writeJSON(w) })
}
```
**Use it to answer**: Where is handler time going? Are we I/O bound or CPU bound?

### 3) Goroutine pipeline (handoffs + queues)
```go
ctx, t := trace.NewTask(context.Background(), "batch")
defer t.End()
trace.WithRegion(ctx, "enqueue", func(){ jobs <- item })
trace.WithRegion(ctx, "wait_result", func(){ <-done })
```
**Use it to answer**: Where do we wait—enqueue, dequeue, or result join? Which stage is the bottleneck?

### 4) Mutex contention around critical section
```go
trace.WithRegion(ctx, "Mx.Lock.acquire", func(){ mu.Lock() })
trace.WithRegion(ctx, "critical", func(){ mutateSharedState() })
trace.WithRegion(ctx, "Mx.Unlock", func(){ mu.Unlock() })
```
**Use it to answer**: Is the lock contended? Is the critical region too large?

### 5) Channel hot path (send/receive)
```go
trace.WithRegion(ctx, "chan.send", func(){ ch <- payload })
trace.WithRegion(ctx, "chan.recv", func(){ x := <-ch; _ = x })

```
**Use it to answer**: Are we backpressured on send or starved on receive?

### 6) Syscall-heavy section (e.g., BPF/IO)
```go
trace.WithRegion(ctx, "syscall.readBPF", func(){ readBPF() })
trace.WithRegion(ctx, "syscall.flush", func(){ flush() })
```
**Use it to answer**: How much tick time is lost to syscalls? Which call dominates?

## Minimal end-to-end example
```go
func main() {
  // serve pprof so you can curl /debug/pprof/trace
  go func() {
    mux := http.NewServeMux()
    mux.HandleFunc("/debug/pprof/", http.DefaultServeMux.ServeHTTP)
    http.ListenAndServe("127.0.0.1:8888", mux)
  }()

  var mx sync.Mutex
  go func() {
    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()
    for {
      ctx, t := trace.NewTask(context.Background(), "tick")
      trace.WithRegion(ctx, "waitTicker", func(){ <-ticker.C })
      trace.WithRegion(ctx, "Mx.Lock.acquire", func(){ mx.Lock() })
      trace.WithRegion(ctx, "UpdateCritical", func(){ time.Sleep(2*time.Millisecond) })
      trace.WithRegion(ctx, "Mx.Unlock", func(){ mx.Unlock() })
      t.End()
    }
  }()

  select {}
}
```