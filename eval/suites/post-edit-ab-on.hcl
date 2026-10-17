// Post-edit checks A/B suite — "on" arm.
//
// This suite and post-edit-ab-off.hcl carry byte-identical tasks; the
// only difference is the suite-level tools.post_edit block. This arm
// runs `go vet ./...` after every successful edit to a .go file, so a
// compile error introduced by one edit comes back in that edit's tool
// result. The off arm has no checks: the model gets no compiler feedback
// at all, because neither arm enables run_command.
//
// Each task is a signature change whose callers span several packages,
// so a partial edit leaves the module failing to compile. The judge
// builds, vets and tests the module.
//
// A/B recipe (run each arm twice; compare pass rate and turns per run):
//   ANTHROPIC_API_KEY=... ./stirrup-eval run \
//       --suite eval/suites/post-edit-ab-off.hcl --output results/pe-off-1
//   ANTHROPIC_API_KEY=... ./stirrup-eval run \
//       --suite eval/suites/post-edit-ab-on.hcl --output results/pe-on-1
//   (repeat with -2 output directories)
//   ./stirrup-eval compare -baseline results/pe-off-1/result.json \
//       -current results/pe-on-1/result.json
//
// Opt-in, live-provider, no CI baseline. Requires a Go toolchain on the
// host, since the tasks run on the local executor. Pass --model to test
// other models; it overrides the pinned model_router.

suite "post-edit-ab-on" {
  description = "A/B arm with tools.post_edit running go vet after each .go edit. Pair with post-edit-ab-off; identical tasks, differing only in tools.post_edit."

  run_config {
    mode      = "execution"
    max_turns = 30

    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_API_KEY"
    }

    model_router {
      type     = "static"
      provider = "anthropic"
      model    = "claude-haiku-5-5"
    }

    permission_policy {
      type = "allow-all"
    }

    tools {
      built_in = ["read_file", "list_directory", "find_files", "grep_files", "edit_file"]

      post_edit {
        glob            = "*.go"
        command         = "go vet ./..."
        timeout_seconds = 120
      }
    }
  }

  task "area-returns-error" {
    description = "Change Shape.Area to return (float64, error) and propagate the error through the report package and the command. Callers live in three packages; a missed caller fails compilation."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      This workspace is a Go module. Change the Shape interface in shapes/shape.go so that Area returns (float64, error), and make Circle and Rect return an error when a dimension is negative. Propagate the error through every caller in the module: report.Total must return (float64, error) and report.Lines must return ([]string, error), and cmd/shapes/main.go must exit with status 1 on an error. The tests in shapes/shape_test.go describe the expected behaviour. The whole module must compile and its tests must pass. Commands cannot be run in this workspace.
    EOT

    file "go.mod" {
      content = <<-EOT
        module example.com/shapes

        go 1.22
      EOT
    }

    file "shapes/shape.go" {
      content = <<-EOT
        package shapes

        // Shape is a plane figure.
        type Shape interface {
            Name() string
            Area() float64
        }
      EOT
    }

    file "shapes/circle.go" {
      content = <<-EOT
        package shapes

        import "math"

        type Circle struct{ R float64 }

        func (c Circle) Name() string { return "circle" }

        func (c Circle) Area() float64 { return math.Pi * c.R * c.R }
      EOT
    }

    file "shapes/rect.go" {
      content = <<-EOT
        package shapes

        type Rect struct{ W, H float64 }

        func (r Rect) Name() string { return "rect" }

        func (r Rect) Area() float64 { return r.W * r.H }
      EOT
    }

    file "shapes/shape_test.go" {
      content = <<-EOT
        package shapes

        import "testing"

        func TestAreaRejectsNegativeDimensions(t *testing.T) {
            for _, s := range []Shape{Circle{R: -1}, Rect{W: -1, H: 2}, Rect{W: 2, H: -1}} {
                if _, err := s.Area(); err == nil {
                    t.Errorf("%s: expected an error for a negative dimension", s.Name())
                }
            }
        }

        func TestAreaValues(t *testing.T) {
            got, err := Rect{W: 2, H: 3}.Area()
            if err != nil || got != 6 {
                t.Errorf("Rect{2,3}.Area() = %v, %v; want 6, nil", got, err)
            }
        }
      EOT
    }

    file "report/report.go" {
      content = <<-EOT
        package report

        import (
            "fmt"

            "example.com/shapes/shapes"
        )

        // Total sums the areas of ss.
        func Total(ss []shapes.Shape) float64 {
            var sum float64
            for _, s := range ss {
                sum += s.Area()
            }
            return sum
        }

        // Lines renders one line per shape.
        func Lines(ss []shapes.Shape) []string {
            out := make([]string, 0, len(ss))
            for _, s := range ss {
                out = append(out, fmt.Sprintf("%s: %.2f", s.Name(), s.Area()))
            }
            return out
        }
      EOT
    }

    file "report/report_test.go" {
      content = <<-EOT
        package report

        import (
            "testing"

            "example.com/shapes/shapes"
        )

        func TestTotalPropagatesError(t *testing.T) {
            if _, err := Total([]shapes.Shape{shapes.Rect{W: 1, H: 1}, shapes.Circle{R: -2}}); err == nil {
                t.Fatal("expected Total to return the shape error")
            }
        }

        func TestLines(t *testing.T) {
            lines, err := Lines([]shapes.Shape{shapes.Rect{W: 2, H: 2}})
            if err != nil || len(lines) != 1 || lines[0] != "rect: 4.00" {
                t.Fatalf("Lines = %q, %v", lines, err)
            }
        }
      EOT
    }

    file "cmd/shapes/main.go" {
      content = <<-EOT
        package main

        import (
            "fmt"

            "example.com/shapes/report"
            "example.com/shapes/shapes"
        )

        func main() {
            ss := []shapes.Shape{shapes.Circle{R: 1}, shapes.Rect{W: 2, H: 3}}
            for _, line := range report.Lines(ss) {
                fmt.Println(line)
            }
            fmt.Printf("total: %.2f\n", report.Total(ss))
        }
      EOT
    }

    judge {
      type    = "test-command"
      command = "go build ./... && go vet ./... && go test ./..."
    }
  }

  task "store-get-takes-context" {
    description = "Change store.Get to take a context and return an error instead of a found flag; update the cache wrapper, the HTTP handler and the command. A missed caller fails compilation."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      This workspace is a Go module. Change (*store.Store).Get in store/store.go from Get(key string) (string, bool) to Get(ctx context.Context, key string) (string, error), returning the exported sentinel error store.ErrNotFound when the key is absent. Update every caller in the module: cache.Cache.Lookup must take a context.Context as its first parameter and return (string, error), and api.Handler must pass the request's context and answer 404 when the error is store.ErrNotFound. The tests describe the expected behaviour. The whole module must compile and its tests must pass. Commands cannot be run in this workspace.
    EOT

    file "go.mod" {
      content = <<-EOT
        module example.com/kv

        go 1.22
      EOT
    }

    file "store/store.go" {
      content = <<-EOT
        package store

        import "sync"

        // Store is an in-memory key-value store.
        type Store struct {
            mu   sync.Mutex
            data map[string]string
        }

        func New() *Store { return &Store{data: map[string]string{}} }

        func (s *Store) Put(key, value string) {
            s.mu.Lock()
            defer s.mu.Unlock()
            s.data[key] = value
        }

        func (s *Store) Get(key string) (string, bool) {
            s.mu.Lock()
            defer s.mu.Unlock()
            v, ok := s.data[key]
            return v, ok
        }
      EOT
    }

    file "store/store_test.go" {
      content = <<-EOT
        package store

        import (
            "context"
            "errors"
            "testing"
        )

        func TestGet(t *testing.T) {
            s := New()
            s.Put("a", "1")
            if v, err := s.Get(context.Background(), "a"); err != nil || v != "1" {
                t.Fatalf("Get(a) = %q, %v", v, err)
            }
            if _, err := s.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
                t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
            }
        }
      EOT
    }

    file "cache/cache.go" {
      content = <<-EOT
        package cache

        import "example.com/kv/store"

        // Cache fronts a store with a fixed prefix.
        type Cache struct {
            Prefix string
            Store  *store.Store
        }

        func (c Cache) Lookup(key string) (string, bool) {
            return c.Store.Get(c.Prefix + key)
        }
      EOT
    }

    file "cache/cache_test.go" {
      content = <<-EOT
        package cache

        import (
            "context"
            "errors"
            "testing"

            "example.com/kv/store"
        )

        func TestLookup(t *testing.T) {
            s := store.New()
            s.Put("p:k", "v")
            c := Cache{Prefix: "p:", Store: s}
            if v, err := c.Lookup(context.Background(), "k"); err != nil || v != "v" {
                t.Fatalf("Lookup(k) = %q, %v", v, err)
            }
            if _, err := c.Lookup(context.Background(), "x"); !errors.Is(err, store.ErrNotFound) {
                t.Fatalf("Lookup(x) error = %v", err)
            }
        }
      EOT
    }

    file "api/handler.go" {
      content = <<-EOT
        package api

        import (
            "net/http"

            "example.com/kv/cache"
        )

        // Handler serves GET /?key=<key>.
        type Handler struct{ Cache cache.Cache }

        func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
            v, ok := h.Cache.Lookup(r.URL.Query().Get("key"))
            if !ok {
                http.NotFound(w, r)
                return
            }
            _, _ = w.Write([]byte(v))
        }
      EOT
    }

    file "api/handler_test.go" {
      content = <<-EOT
        package api

        import (
            "net/http"
            "net/http/httptest"
            "testing"

            "example.com/kv/cache"
            "example.com/kv/store"
        )

        func TestHandler(t *testing.T) {
            s := store.New()
            s.Put("k", "v")
            h := Handler{Cache: cache.Cache{Store: s}}

            rec := httptest.NewRecorder()
            h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?key=k", nil))
            if rec.Code != http.StatusOK || rec.Body.String() != "v" {
                t.Fatalf("found: %d %q", rec.Code, rec.Body.String())
            }

            rec = httptest.NewRecorder()
            h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?key=nope", nil))
            if rec.Code != http.StatusNotFound {
                t.Fatalf("missing: %d", rec.Code)
            }
        }
      EOT
    }

    file "cmd/kv/main.go" {
      content = <<-EOT
        package main

        import (
            "fmt"
            "os"

            "example.com/kv/cache"
            "example.com/kv/store"
        )

        func main() {
            s := store.New()
            s.Put("greeting", "hello")
            c := cache.Cache{Store: s}
            v, ok := c.Lookup("greeting")
            if !ok {
                fmt.Println("not found")
                os.Exit(1)
            }
            fmt.Println(v)
        }
      EOT
    }

    judge {
      type    = "test-command"
      command = "go build ./... && go vet ./... && go test ./..."
    }
  }
}
