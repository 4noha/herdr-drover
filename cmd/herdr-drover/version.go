package main

// version はビルド時に -ldflags で注入する:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/herdr-drover
//
// 未注入（go run / 素の go build）では "dev"。cm の Phase1 版可視化と同じく
// RegisterPCVersion で Firestore へも報告する（agent.go）＝Web から
// 旧バイナリ常駐を検出できるようにする土台。
var version = "dev"
