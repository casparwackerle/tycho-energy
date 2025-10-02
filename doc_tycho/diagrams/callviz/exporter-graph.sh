#!/usr/bin/env bash

go-callvis -group pkg,type -ignore "vendor|third_party|internal/telemetry"   -format svg ~/Documents/git/tycho-energy/cmd/exporter/exporter.go > ~/Documents/git/tycho-energy/doc_tycho/diagrams/callviz/exporter.svg