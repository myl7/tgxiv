.PHONY: build web test clean

build: web
	go build -trimpath -ldflags "-s -w" -o bin/tgxiv .

web:
	pnpm --dir web install
	pnpm --dir web build
	node web/scripts/pack-dist.mjs

test:
	go test ./...

clean:
	rm -rf bin/ web/out/ web/.next/
