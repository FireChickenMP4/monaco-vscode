# 常用命令（Linux/macOS；Windows 直接看 README 里的 PowerShell 版本）
BIN := bin/examd
DATA ?= ./data
WEB ?= ./public

.PHONY: all build build-web build-server test e2e load run seed clean

all: build

build: build-web build-server

build-web:
	cd web && npm ci && npm run build

build-server:
	cd server && go build -o ../$(BIN) ./cmd/examd

test:
	cd server && go test ./...

e2e:
	cd tests/e2e && npm ci && node run.mjs

load:
	cd tests/e2e && node loadtest.mjs -n 70 -soak 60

run: build-server
	./$(BIN) serve -data $(DATA) -web $(WEB) -problems ./problems

seed: build-server
	./$(BIN) addstudent -data $(DATA) -no 2026001 -name demo

clean:
	rm -rf bin data public/assets public/index.html
