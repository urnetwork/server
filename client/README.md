# Client development guidelines

This is the same concurrency guidelines as the `connect` package.

- Non-view objects should be written to be concurrently accessed, except:
  -- Read-only data objects
  -- View object (e.g. view controllers) that assume a single main thread access
  -- Any object that assumes a single thread ownership, which must be clearly documented

## When changing your version of Go

```
$BRINGYOUR_HOME/bringyour/clientgo install golang.org/x/mobile/cmd/gomobile@latest
$BRINGYOUR_HOME/bringyour/clientgo gomobile init
```
