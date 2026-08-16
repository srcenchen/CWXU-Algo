import re
import unittest
from pathlib import Path


DEPLOY = Path(__file__).resolve().parents[1]


def read(relative: str) -> str:
    return (DEPLOY / relative).read_text(encoding="utf-8")


class ContainerRuntimeTests(unittest.TestCase):
    def test_compose_has_complete_healthy_stack(self):
        compose = read("compose.yaml")
        services = {
            "frontend",
            "gateway",
            "user",
            "core-data",
            "agent",
            "postgres",
            "redis",
            "rabbitmq",
            "consul",
            "nginx",
        }
        for service in services:
            self.assertRegex(compose, rf"(?m)^  {re.escape(service)}:\s*$")
        self.assertEqual(compose.count("healthcheck:"), len(services))
        self.assertIn("condition: service_healthy", compose)

    def test_only_nginx_publishes_a_host_port(self):
        compose = read("compose.yaml")
        self.assertEqual(compose.count("ports:"), 1)
        self.assertIn(
            '"${GOALGO_HTTP_BIND:-0.0.0.0}:${GOALGO_HTTP_PORT:-8988}:8080"',
            compose,
        )
        self.assertNotRegex(compose, r"(?m)^\s+- [\"']?\d{2,5}:\d{2,5}")

    def test_compose_uses_release_images_and_host_mounts(self):
        compose = read("compose.yaml")
        for variable in (
            "FRONTEND_IMAGE",
            "GATEWAY_IMAGE",
            "USER_IMAGE",
            "CORE_DATA_IMAGE",
            "AGENT_IMAGE",
        ):
            self.assertIn("${" + variable + "?", compose)
        self.assertIn("${GOALGO_ROOT:-/opt/goalgo}", compose)
        for host_path in (
            "/data/postgres",
            "/data/redis",
            "/data/rabbitmq",
            "/data/consul",
            "/config",
            "/secrets",
        ):
            self.assertIn(host_path, compose)

    def test_compose_secret_and_backup_mount_contract_is_nonroot_safe(self):
        compose = read("compose.yaml")
        self.assertNotIn("/run/secrets/goalgo:ro", compose)
        self.assertIn("backup_encryption_key:/run/secrets/backup_encryption_key:ro", compose)
        self.assertIn("/data/backups:/var/lib/goalgo/backups", compose)
        self.assertIn("CWXU_BACKUP_WORK_DIR: /var/lib/goalgo/backups", compose)
        self.assertIn("CWXU_BACKUP_ENCRYPTION_KEY_FILE: /run/secrets/backup_encryption_key", compose)
        provision = read("scripts/provision-root.sh")
        self.assertIn("10001:10001", provision)
        for owner in ("999:999", "999:1000", "100:101", "100:1000"):
            self.assertIn(owner, provision)
        self.assertIn("data/backups", provision)

    def test_backend_dockerfile_has_exact_targets_and_commands(self):
        dockerfile = read("docker/backend.Dockerfile")
        for target, source, command in (
            ("gateway", "./cmd/gateway", "/app/gateway"),
            ("user", "./app/user/cmd/user", "/app/user"),
            ("core-data", "./app/core_data/cmd/core_data", "/app/core-data"),
            ("agent", "./app/agent/cmd/agent", "/app/agent"),
        ):
            self.assertRegex(dockerfile, rf"(?m)^FROM .* AS {target}$")
            self.assertIn(source, dockerfile)
            self.assertIn(command, dockerfile)
        self.assertIn("postgresql-client-18", dockerfile)
        self.assertIn("zstd", dockerfile)
        self.assertRegex(dockerfile, r"(?m)^USER goalgo$")
        self.assertNotRegex(dockerfile, r"(?im)^COPY .*config\.ya?ml")
        self.assertIn("gettext-base", dockerfile)
        self.assertIn("render-config.sh", dockerfile)
        self.assertIn("chown goalgo:goalgo /run/goalgo", dockerfile)

    def test_runtime_renderer_expands_mounted_templates(self):
        renderer = read("docker/render-config.sh")
        self.assertIn("envsubst", renderer)
        self.assertIn("/run/goalgo/config.yaml", renderer)
        self.assertIn('exec "$@" -conf', renderer)
        self.assertIn("chmod 0600 /run/goalgo/config.yaml", renderer)

    def test_frontend_supports_separate_named_context(self):
        dockerfile = read("docker/frontend.Dockerfile")
        self.assertIn("COPY --from=frontend-source package.json package-lock.json", dockerfile)
        self.assertIn("COPY --from=frontend-source . .", dockerfile)
        self.assertIn("COPY --from=shared-source . /shared", dockerfile)
        self.assertIn("rm -rf /src/shared", dockerfile)
        self.assertIn("ln -s /shared /src/shared", dockerfile)
        self.assertIn("npm ci", dockerfile)
        self.assertIn("npm run build", dockerfile)
        self.assertIn("USER nginx", dockerfile)

    def test_nginx_preserves_api_spa_and_seo_routes(self):
        nginx = read("docker/nginx.conf")
        self.assertRegex(nginx, r"location \^~ /api/")
        self.assertIn("proxy_pass http://gateway:8080/v1/;", nginx)
        self.assertIn("proxy_pass http://frontend:8080;", nginx)
        self.assertIn("rewrite ^ /index.html break;", nginx)
        self.assertIn("X-Original-URI $request_uri", nginx)
        self.assertIn("/v1/user/seo/html", nginx)
        for route in (
            "/blog/",
            "question-bank/detail/",
            "/problemset",
            "profile",
            "/blog-plaza",
            "p/[A-Za-z0-9]",
            "/tools/paste",
            "/sitemap.xml",
        ):
            self.assertIn(route, nginx)

    def test_nginx_only_preserves_sanitized_forwarded_headers_from_trusted_proxies(self):
        nginx = read("docker/nginx.conf")
        self.assertIn("geo $goalgo_trusted_proxy", nginx)
        self.assertIn("$http_x_forwarded_proto", nginx)
        self.assertIn("~^1:(https?)$", nginx)
        self.assertIn("$http_x_forwarded_host", nginx)
        self.assertIn("$goalgo_forwarded_proto", nginx)
        self.assertIn("$goalgo_forwarded_host", nginx)
        self.assertNotIn("proxy_set_header X-Forwarded-Proto $scheme;", nginx)

    def test_templates_use_compose_dns_without_secrets(self):
        configs = "\n".join(
            read(path)
            for path in (
                "config/gateway.yaml",
                "config/user.yaml",
                "config/core-data.yaml",
                "config/agent.yaml",
            )
        )
        for dns_name in ("consul:8500",):
            self.assertIn(dns_name, configs)
        for variable in ("USER_DATABASE_DSN", "CORE_DATABASE_DSN", "REDIS_ADDR", "REDIS_PASSWORD", "AMQP_DSN"):
            self.assertIn("${" + variable + "}", configs)
        self.assertNotRegex(configs, r"postgres://\$\{")
        self.assertNotRegex(configs, r"amqp://\$\{")
        forbidden = (
            "BEGIN RSA PRIVATE KEY",
            "BEGIN PRIVATE KEY",
            "admin123",
            "password=cwxu",
            "cwxu-algo:cwxu-algo",
            "103.24.",
            "192.168.",
            "127.0.0.1",
        )
        checked = configs + read("env.example") + read("release.env.example")
        for literal in forbidden:
            self.assertNotIn(literal, checked)

    def test_ignore_file_excludes_secrets_and_local_artifacts(self):
        ignored = read("docker/backend.Dockerfile.dockerignore")
        for pattern in ("**/.env", "**/secrets", "**/config.yaml", ".git", "bin", "data"):
            self.assertIn(pattern, ignored)
        self.assertIn("app/**/configs/config.yaml", ignored)
        self.assertIn(".goalgo-*", ignored)

    def test_rabbitmq_reads_mounted_password_without_file_environment_extension(self):
        compose = read("compose.yaml")
        self.assertNotIn("RABBITMQ_DEFAULT_PASS_FILE", compose)
        self.assertIn("rabbitmq-entrypoint.sh", compose)
        self.assertIn("check_port_connectivity", compose)
        entrypoint = read("docker/rabbitmq-entrypoint.sh")
        self.assertIn("RABBITMQ_DEFAULT_PASS=", entrypoint)
        self.assertIn("/run/secrets/rabbitmq_password", entrypoint)
        self.assertIn("docker-entrypoint.sh", entrypoint)

    def test_postgres_initializes_all_current_databases(self):
        init = read("config/postgres-init.sh")
        for database in ("algo_user", "algo_core_data", "sanenchen", "support"):
            self.assertIn(database, init)

    def test_infrastructure_images_use_exact_version_tags(self):
        compose = read("compose.yaml")
        for image in (
            "pgvector/pgvector:0.8.1-pg18-bookworm",
            "redis:8.2.1-alpine",
            "rabbitmq:4.2.0-management-alpine",
            "hashicorp/consul:1.21.5",
            "nginxinc/nginx-unprivileged:1.29.1-alpine",
        ):
            self.assertIn("image: " + image, compose)

    def test_release_contract_accepts_digest_or_latest(self):
        release = read("release.env.example")
        self.assertEqual(release.count("-latest"), 5)
        compose = read("compose.yaml")
        self.assertNotIn(":latest", compose)


if __name__ == "__main__":
    unittest.main()
