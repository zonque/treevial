# Separate-module consumers

This directory is its own Go module, depending on `github.com/holoplot/treevial`
through a `replace` directive. It exists to prove what the layout is for: that
a client repository and a server repository can each import only their own side
of treevial and build.

`settings` holds the baseline struct the two sides share: the server walks it
into a tree, the client applies the tree back into it. When the two sides live
in different repositories, a package like this is what they both depend on,
alongside treevial itself.

`clientside` imports `treevial`, `treevial/client`, `treevial/structtree` and `settings` —
no server package, no object store. `serverside` imports `treevial`, `treevial/server`,
`treevial/objects`, `treevial/structtree` and `settings` — no client package. Neither can reach `treevial/internal/...`, so if a public API ever
leaked an internal type these would stop compiling.

`TestSeparateModuleConsumersBuild` in the main module builds this one.

In a real repository the `replace` line goes away and the dependency is an
ordinary `require github.com/holoplot/treevial v…`.
