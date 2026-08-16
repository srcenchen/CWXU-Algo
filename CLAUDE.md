# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Wuxi University Algorithm Association website backend - a Go microservices application using the Kratos framework. The system crawls problem submissions from competitive programming platforms (NowCoder, AtCoder, CodeForces, LuoGu) and provides statistics and ranking functionality.

## Build Commands

```bash
# Install required tools (run once on setup)
make init

# Generate API from protobuf definitions
make api

# Generate config from internal proto
make config

# Run wire dependency injection code generation
make generate

# Run all generation steps (api + config + wire)
make all

# Build all binaries to ./bin/
make build
```

## Architecture

### Microservices

The application consists of three main services:

1. **Gateway** (`app/gateway`) - API gateway using Kratos framework with middleware for circuit breaking, CORS, JWT auth, logging, and tracing. Routes requests to backend services.

2. **User** (`app/user`) - User management service handling authentication and user profile operations.

3. **Core Data** (`app/core_data`) - Core data service handling:
   - Submit logs from various OJ platforms
   - Spider/crawler functionality
   - Statistics and ranking
   - RabbitMQ event consumption

### Technology Stack

- **Framework**: go-kratos/kratos (v2)
- **Database**: PostgreSQL with GORM
- **Cache**: Redis
- **Message Queue**: RabbitMQ (amqp)
- **Service Discovery**: Consul
- **Dependency Injection**: Wire (google/wire)
- **Serialization**: Protobuf

### Directory Structure

```
app/
├── common/          # Shared code across services
│   ├── conf/        # Configuration (protobuf)
│   ├── data/        # Data layer initialization (gorm, redis)
│   ├── discovery/   # Service discovery (consul)
│   ├── event/       # Event handling (rabbitmq)
│   └── utils/       # Utilities (auth, gob encoding)
├── gateway/         # API gateway service
├── user/            # User service
│   ├── cmd/user/    # Entry point (main.go, wire.go)
│   ├── configs/     # Service config (config.yaml)
│   └── internal/    # Kratos standard layout
│       ├── biz/     # Business logic
│       ├── data/    # Data layer (models, dal)
│       ├── server/  # HTTP/gRPC servers
│       └── service/ # gRPC/HTTP implementations
├── core_data/       # Core data service
│   ├── cmd/core_data/
│   ├── configs/
│   ├── internal/
│   │   ├── biz/service/    # SpiderUseCase, Consumer
│   │   ├── data/model/     # SubmitLog, Platform models
│   │   ├── spider/         # Platform spider registry & implementations
│   │   └── task/           # Cron jobs, task scheduling
│   └── task/           # Task providers (SpiderTask, CronTask)
api/
└── core/v1/         # Core API protobuf definitions
    ├── spider/      # Spider service
    ├── submit_log/  # Submit log service
    └── statistic/   # Statistics service
```

### Kratos Standard Layout

Each service follows the Kratos framework's internal directory layout:

- **cmd**: Application entry point with wire.go for dependency injection
- **internal/biz**: Business logic layer (UseCases)
- **internal/data**: Data access layer (models, database clients)
- **internal/server**: Server setup (HTTP, gRPC)
- **internal/service**: gRPC service implementations

### Spider Plugin System

Platform spiders are registered using a plugin pattern:

1. Implement `spider.SubmitLogFetcher` interface with `FetchSubmitLog()` method
2. Implement `spider.Provider` interface with `Name()` method
3. Register using `spider.Register()` in init()
4. Available platforms: NowCoder, AtCoder, CodeForces, LuoGu

### Event Flow

1. Services publish spider events to RabbitMQ queue "spider"
2. Core Data Consumer receives events and calls `SpiderUseCase.LoadData()`
3. SpiderUseCase fetches from registered platform providers
4. Results saved to database with UPSERT (on conflict ignore)
5. Redis cache invalidated (SCAN for pattern-based cache clearing)

### Configuration

Each service has its own `configs/config.yaml` containing:
- Server: HTTP/gRPC addresses, timeouts
- Service discovery DSN (Consul)
- AMQP DSN (RabbitMQ)
- Data: Database connection (PostgreSQL)
- Redis: connection settings

SMTP, Agent/AI, Upyun, OJ, PayFM, and backup scheduling are runtime site settings from `site_configs` + Redis, not YAML fallbacks.

Any config proto, example, template, or loader change must keep `deploy/config`, container render/provision, `goalgo-ops` scaffold/doctor, and their contract tests synchronized.

### Adding a New Platform Spider

1. Create new file in `app/core_data/internal/spider/platform/`
2. Implement `spider.SubmitLogFetcher` interface
3. Call `spider.Register()` in init()
4. Add platform constant to `app/core_data/internal/spider/registry.go`

### Wire Dependency Injection

- Edit `wire.go` files in service cmd directories
- Run `make generate` to regenerate `wire_gen.go`
- Wire builds the dependency graph using ProviderSets from each layer
