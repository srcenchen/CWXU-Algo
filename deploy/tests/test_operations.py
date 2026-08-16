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
        self.assertIn("AdminCreated", main)
        self.assertIn("opsadmin.Bootstrap", main)
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
        self.assertIn("rollbackFiles", runtime)
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
        self.assertIn("postgresql-client", restore)

    def test_shell_scripts_parse(self):
        scripts = sorted((DEPLOY / "scripts").glob("*.sh"))
        result = subprocess.run(["sh", "-n", *map(str, scripts)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)


class WorkflowTests(unittest.TestCase):
    def test_ci_is_unprivileged_and_runs_on_pr_and_main(self):
        workflow = read(".github/workflows/ci.yml")
        self.assertRegex(workflow, r"(?m)^\s*pull_request:\s*$")
        self.assertRegex(workflow, r"(?m)^\s*push:\s*$")
        self.assertIn("main", workflow)
        self.assertIn("contents: read", workflow)
        self.assertNotIn("ACR_PASSWORD", workflow)
        self.assertNotIn("self-hosted", workflow)

    def test_images_only_follows_successful_main_ci_and_uses_acr_secrets(self):
        workflow = read(".github/workflows/images.yml")
        self.assertIn("workflow_run:", workflow)
        self.assertIn("CI", workflow)
        self.assertIn("completed", workflow)
        self.assertIn("conclusion == 'success'", workflow)
        self.assertIn("workflow_run.event == 'push'", workflow)
        self.assertIn("head_branch == 'main'", workflow)
        self.assertIn("ACR_USERNAME", workflow)
        self.assertIn("ACR_PASSWORD", workflow)
        self.assertIn("registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo", workflow)
        for service in ("frontend", "gateway", "user", "core-data", "agent"):
            self.assertIn(service, workflow)
        self.assertIn("backendCommit", workflow)
        self.assertIn("frontendCommit", workflow)
        self.assertIn("release-manifest", workflow)
        self.assertEqual(workflow.count("provenance: false"), 5)
        self.assertEqual(workflow.count("sbom: false"), 5)
        for service in ("frontend", "gateway", "user", "core-data", "agent"):
            self.assertIn(f":{service}-latest", workflow)
            self.assertIn(f":{service}-sha-", workflow)

    def test_production_is_tag_only_approved_serial_self_hosted_deploy(self):
        workflow = read(".github/workflows/production.yml")
        self.assertRegex(workflow, r"(?s)push:.*tags:.*v\*")
        self.assertIn("environment: production", workflow)
        self.assertIn("self-hosted", workflow)
        self.assertIn("goalgo-production", workflow)
        self.assertIn("cancel-in-progress: false", workflow)
        self.assertIn("actions: read", workflow)
        self.assertIn("deploy/scripts/deploy.sh", workflow)
        self.assertIn("runs[0].status", workflow)
        self.assertIn("runs[0].conclusion", workflow)
        self.assertNotIn("docker build", workflow)
        self.assertNotIn("ACR_PASSWORD", workflow)

    def test_workflows_have_restrictive_permissions(self):
        for name in ("ci", "images", "production"):
            workflow = read(f".github/workflows/{name}.yml")
            self.assertRegex(workflow, r"(?m)^permissions:\s*$")
            self.assertNotIn("write-all", workflow)


if __name__ == "__main__":
    unittest.main()
