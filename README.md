# Polymarket Demo


## Dependencies
* [Foundry](https://www.getfoundry.sh/)
* [Golang](https://go.dev/doc/install)


## Build
```
cd contracts

forge build
```


## Run
```
cd exchange

# 配置exchange/config.json

# 正常流程
go run cmd/normal/main.go

# 争议流程
go run cmd/dispute/main.go

# nonce验证
go run cmd/nonce_test/main.go

# 三种错误类型测试
go run cmd/match_types/main.go

```


## Docs
* [架构分析](./docs/01-architecture-analysis.md)
* [测试流程](./docs/04-flow-diagram.md)
* [测试中的坑](./docs/05-pitfalls.md)


## Logs
* [正常流程](./logs/normal.log)
* [争议流程](./logs/dispute.log)
* [nonce验证](./logs/nonce.log)
* [三种撮合类型](./logs/match_types.log)


