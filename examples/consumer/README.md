# Separate-module consumers

This directory is its own Go module, depending on `github.com/holoplot/gats`
through a `replace` directive. It exists to prove what the layout is for: that
a client repository and a server repository can each import only their own side
of gats and build.

`clientside` imports `gats` and `gats/client` — no server package, no object
store. `serverside` imports `gats`, `gats/server` and `gats/objects` — no
client package. Neither can reach `gats/internal/...`, so if a public API ever
leaked an internal type these would stop compiling.

`TestSeparateModuleConsumersBuild` in the main module builds this one.

In a real repository the `replace` line goes away and the dependency is an
ordinary `require github.com/holoplot/gats v…`.
