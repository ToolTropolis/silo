# Vault configuration for the local dev stack.
#
# This is NOT `-dev` mode. Dev mode keeps everything in memory, so a plain
# `docker restart` destroys every per-project SSE key while the tenant registry
# still references it — a silent partial-loss state where projects look healthy
# but their key material is gone. File storage makes keys survive restarts and
# `docker compose down`.
#
# Still DEV-ONLY: TLS is disabled and the unseal keys are written to a file in
# the container. Production uses TLS, a real storage backend (Raft/Consul/cloud),
# and auto-unseal via a cloud KMS — never a file on disk.

storage "file" {
  path = "/vault/file"
}

listener "tcp" {
  address     = "0.0.0.0:8200"
  tls_disable = true
}

# Required in server mode so Vault knows how clients reach it.
api_addr = "http://127.0.0.1:8200"

# Allow the unseal/init state to be inspected by the bootstrap script.
disable_mlock = true
ui            = false
