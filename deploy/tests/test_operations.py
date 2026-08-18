import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO = Path(__file__).resolve().parents[2]
DEPLOY = REPO / "deploy"


def read(relative: str) -> str:
    return (REPO / relative).read_text(encoding="utf-8")


class OperationalScriptTests(unittest.TestCase):
    def test_all_commands_use_shared_library_and_are_executable(self):
        for name in ("start", "stop", "restart", "status", "logs", "doctor", "deploy", "rollback"):
            path = DEPLOY / "scripts" / f"{name}.sh"
            self.assertTrue(path.exists(), name)
            self.assertTrue(os.access(path, os.X_OK), name)
            self.assertIn("lib.sh", path.read_text(encoding="utf-8"))

    def test_library_always_uses_both_root_environment_files_and_lock(self):
        library = read("deploy/scripts/lib.sh")
        self.assertIn('GOALGO_ROOT=${GOALGO_ROOT:-/opt/goalgo}', library)
        self.assertIn('"$GOALGO_ROOT/.env"', library)
        self.assertIn('"$GOALGO_ROOT/release.env"', library)
        self.assertIn("--env-file", library)
        self.assertIn("flock", library)
        self.assertIn("/run/lock/goalgo-ops.lock", library)

    def test_release_validation_accepts_digest_or_latest(self):
        library = read("deploy/scripts/lib.sh")
        self.assertIn("registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo", library)
        self.assertIn("@sha256:[0-9a-f]{64}", library)
        self.assertIn("(frontend|gateway|user|core-data|agent)-latest", library)
        for variable in ("FRONTEND_IMAGE", "GATEWAY_IMAGE", "USER_IMAGE", "CORE_DATA_IMAGE", "AGENT_IMAGE"):
            self.assertIn(variable, library)

    def test_deploy_is_atomic_and_rolls_back_failed_release(self):
        script = read("deploy/scripts/deploy.sh")
        self.assertIn("release.previous.env", script)
        self.assertIn("atomic_copy", script)
        self.assertIn("compose pull", script)
        self.assertIn("compose up -d --wait", script)
        self.assertIn("smoke_test", script)
        self.assertIn("rollback_release", script)

    def test_provision_generates_rsa_keys_and_secret_environment_without_printing_values(self):
        script = read("deploy/scripts/provision-root.sh")
        self.assertIn("openssl genpkey", script)
        self.assertIn("openssl pkey -pubout", script)
        self.assertIn("jwt_private_key.pem", script)
        self.assertIn("jwt_public_key.pem", script)
        self.assertIn("app.env", script)
        self.assertIn("CWXU_JWT_SECRET", script)
        self.assertIn("CWXU_BACKUP_PG_DSN", script)
        self.assertIn("postgres:5432", script)
        self.assertIn('postgres_user=$(sed -n', script)
        self.assertIn("urllib.parse.quote", script)
        self.assertNotRegex(script, r"(?m)^set -x")
        self.assertIn("chmod 0600", script)
        self.assertIn('chown 10001:10001 "$root/secrets/backup_encryption_key" "$root/secrets/jwt_private_key.pem" "$root/secrets/jwt_public_key.pem"', script)

    def test_gateway_loads_public_key_from_mounted_file(self):
        compose = read("deploy/compose.yaml")
        entrypoint = read("deploy/docker/gateway-entrypoint.sh")
        dockerfile = read("deploy/docker/backend.Dockerfile")
        self.assertIn("jwt_public_key.pem:/run/secrets/jwt_public_key.pem:ro", compose)
        self.assertIn("CWXU_JWT_PUBLIC_KEY", entrypoint)
        self.assertIn("JWT_PUBLIC_KEY_FILE", entrypoint)
        self.assertIn("gateway-entrypoint.sh", dockerfile)

    def test_ops_usage_mentions_init_and_interactive(self):
        main = read("cmd/goalgo-ops/main.go")
        self.assertIn('"init"', main)
        self.assertIn("init [--root]", main)

    def test_install_uses_progress_and_creates_admin(self):
        main = read("cmd/goalgo-ops/main.go")
        self.assertIn("opsprogress", main)
        self.assertIn("opsprogress.New(6", main)
        self.assertIn("AdminCreated", main)
        self.assertIn("opsadmin.Bootstrap", main)
        self.assertIn('"admin-config"', main)
        self.assertIn("LatestTagRelease", main)

    def test_destructive_commands_prompt_when_missing_args(self):
        restore = read("cmd/goalgo-ops/restore.go")
        self.assertIn("opsprompt", restore)
        self.assertIn("RESTORE", restore)
        backup = read("cmd/goalgo-ops/backup.go")
        self.assertIn("opsprompt", backup)
        config = read("cmd/goalgo-ops/config.go")
        self.assertIn("opsprompt", config)

    def test_upgrade_resolves_latest_and_rolls_back_on_failure(self):
        main = read("cmd/goalgo-ops/main.go")
        self.assertIn('"upgrade"', main)
        runtime = read("cmd/goalgo-ops/runtime.go")
        self.assertIn("ResolveLatest", runtime)
        self.assertIn("opsdata", runtime)
        self.assertIn("applyRelease", main)
        self.assertIn("restoreReleaseFiles", main)
        self.assertIn("release.previous.env", runtime)

    def test_uninstall_deletes_all_and_asks_images(self):
        main = read("cmd/goalgo-ops/main.go")
        self.assertIn('"uninstall"', main)
        runtime = read("cmd/goalgo-ops/runtime.go")
        self.assertIn("RemoveAll", runtime)
        self.assertIn('"down"', runtime)
        self.assertIn("registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo:", runtime)

    def test_restore_file_accepts_local_or_url(self):
        restore = read("cmd/goalgo-ops/restore.go")
        self.assertNotIn("useLatest", restore)
        self.assertIn("http://", restore)
        self.assertIn("downloadURL", restore)
        main = read("cmd/goalgo-ops/main.go")
        self.assertNotIn("--latest", main)

    def test_restore_checks_backup_tools(self):
        restore = read("cmd/goalgo-ops/restore.go")
        self.assertIn("pg_restore", restore)
        self.assertIn("zstd", restore)
        self.assertIn("postgresql-client-18", restore)
        self.assertIn("major < 18", restore)
        self.assertIn("CORE_DATA_IMAGE", restore)
        self.assertIn("ContainerToolRunner", restore)
        self.assertIn("resolveRegisteredRoot", restore)
        self.assertIn('"$POSTGRES_USER"', restore)

    def test_install_persists_root_and_runtime_reads_it(self):
        runtime = read("cmd/goalgo-ops/runtime.go")
        self.assertIn("data.Root", runtime)
        self.assertIn("persistInstallRoot", runtime)
        self.assertIn("resolveRegisteredRoot", runtime)
        main = read("cmd/goalgo-ops/main.go")
        self.assertIn("persistInstallRoot", main)
        self.assertLess(main.index("compose.Smoke(ctx)"), main.index("persistInstallRoot(root)"))

    def test_registration_and_lock_are_system_global(self):
        data = read("internal/opsdata/data.go")
        self.assertIn("/var/lib/goalgo-ops/ops.data.json", data)
        self.assertIn("GOALGO_OPS_DATA_FILE", data)
        self.assertNotIn("type Webhook", data)
        main = read("cmd/goalgo-ops/main.go")
        self.assertIn('return "/run/lock/goalgo-ops.lock"', main)

    def test_restore_and_config_are_transactional(self):
        restore = read("cmd/goalgo-ops/restore.go")
        self.assertIn("validateRestoreConfirmation", restore)
        self.assertIn("recoverRestore", restore)
        config = read("cmd/goalgo-ops/config.go")
        self.assertIn("importConfigBundle", config)
        self.assertIn(".goalgo-config-stage-", config)
        self.assertIn("opslock.Acquire", config)

    def test_shell_scripts_parse(self):
        scripts = sorted((DEPLOY / "scripts").glob("*.sh"))
        result = subprocess.run(["sh", "-n", *map(str, scripts)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)


class WorkflowTests(unittest.TestCase):
    def test_backend_images_run_directly_on_main_and_use_acr_secrets(self):
        workflow = read(".github/workflows/back-image.yml")
        self.assertRegex(workflow, r"(?m)^\s*push:\s*$")
        self.assertIn("main", workflow)
        self.assertIn("contents: read", workflow)
        self.assertIn("ACR_USERNAME", workflow)
        self.assertIn("ACR_PASSWORD", workflow)
        self.assertIn("registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo", workflow)
        self.assertNotIn("self-hosted", workflow)
        self.assertNotIn("workflow_run", workflow)
        self.assertNotIn("pull_request", workflow)
        self.assertNotIn("frontend", workflow)
        self.assertNotIn("repository:", workflow)
        self.assertIn("cancel-in-progress: false", workflow)
        # 四个服务并行构建：matrix 同时覆盖四个 target，各自推送 commit 标签
        self.assertIn("strategy:", workflow)
        self.assertIn("matrix:", workflow)
        self.assertIn("fail-fast: false", workflow)
        self.assertIn("target: [gateway, user, core-data, agent]", workflow)
        self.assertIn("${{ matrix.target }}-sha-${{ github.sha }}", workflow)
        # latest 标签在四个镜像全部成功后统一提升（matrix job 全绿才触发）
        self.assertIn("promote-latest", workflow)
        self.assertIn("needs: build", workflow)
        self.assertIn('${target}-latest', workflow)
        self.assertEqual(workflow.count("provenance: false"), 1)
        self.assertEqual(workflow.count("sbom: false"), 1)

    def test_production_workflow_is_preserved_but_disabled(self):
        disabled = REPO / ".github/workflows/production.yml.disabled"
        self.assertTrue(disabled.is_file())
        self.assertFalse((REPO / ".github/workflows/production.yml").exists())
        self.assertFalse((REPO / ".github/workflows/production.yaml").exists())
        workflow = disabled.read_text()
        self.assertIn("environment: production", workflow)
        self.assertIn("deploy/scripts/deploy.sh", workflow)

    def test_workflows_have_restrictive_permissions(self):
        workflows = list((REPO / ".github/workflows").glob("*.yml"))
        workflows += list((REPO / ".github/workflows").glob("*.yaml"))
        self.assertEqual([path.name for path in workflows], ["back-image.yml"])
        workflow = workflows[0].read_text()
        self.assertRegex(workflow, r"(?m)^permissions:\s*$")
        self.assertNotIn("write-all", workflow)


if __name__ == "__main__":
    unittest.main()
