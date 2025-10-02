(source: https://github.com/ondrajz/go-callvis)


### Usage

#### Interactive viewer

To use the interactive view provided by a web server that serves SVG images of focused packages, you can simply run:

`go-callvis <target package>` 

HTTP server is listening on [http://localhost:7878/](http://localhost:7878/) by default, use option `-http="ADDR:PORT"` to change HTTP server address.

#### Render static output

To generate a single output file use option `-file=<file path>` to choose output file destination.

The output format defaults to `svg`, use option `-format=<svg|png|jpg|...>` to pick a different output format.

#### Options

```
Usage of go-callvis:
  -debug
    	Enable verbose log.
  -file string
    	output filename - omit to use server mode
  -cacheDir string
    	Enable caching to avoid unnecessary re-rendering.
  -focus string
    	Focus specific package using name or import path. (default "main")
  -format string
    	output file format [svg | png | jpg | ...] (default "svg")
  -graphviz
    	Use Graphviz's dot program to render images.
  -group string
    	Grouping functions by packages and/or types [pkg, type] (separated by comma) (default "pkg")
  -http string
    	HTTP service address. (default ":7878")
  -ignore string
    	Ignore package paths containing given prefixes (separated by comma)
  -include string
    	Include package paths with given prefixes (separated by comma)
  -limit string
    	Limit package paths to given prefixes (separated by comma)
  -minlen uint
    	Minimum edge length (for wider output). (default 2)
  -nodesep float
    	Minimum space between two adjacent nodes in the same rank (for taller output). (default 0.35)
  -nointer
    	Omit calls to unexported functions.
  -nostd
    	Omit calls to/from packages in standard library.
  -rankdir
        Direction of graph layout [LR | RL | TB | BT] (default "LR")
  -skipbrowser
    	Skip opening browser.
  -tags build tags
    	a list of build tags to consider satisfied during the build. For more information about build tags, see the description of build constraints in the documentation for the go/build package
  -tests
    	Include test code.
  -algo string
        Use specific algorithm for package analyzer: static, cha or rta (default "static")
  -version
    	Show version and exit.
```

Run `go-callvis -h` to list all supported options.

## Reference guide

Here you can find descriptions for various types of output.

### Packages / Types

|Represents  | Style|
|----------: | :-------------|
|`focused`   | **blue** color|
|`stdlib`    | **green** color|
|`other`     | **yellow** color|

### Functions / Methods

|Represents   | Style|
|-----------: | :--------------|
|`exported`   | **bold** border|
|`unexported` | **normal** border|
|`anonymous`  | **dotted** border|

### Calls

|Represents   | Style|
|-----------: | :-------------|
|`internal`   | **black** color|
|`external`   | **brown** color|
|`static`     | **solid** line|
|`dynamic`    | **dashed** line|
|`regular`    | **simple** arrow|
|`concurrent` | arrow with **circle**|
|`deferred`   | arrow with **diamond**|
