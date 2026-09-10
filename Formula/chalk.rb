class Chalk < Formula
  desc "Shared blackboard MCP server for agent coordination"
  homepage "https://github.com/nburns/chalk"
  url "https://github.com/nburns/chalk/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0019dfc4b32d63c1392aa264aed2253c1e0c2fb09216f8e2cc269bbfb8bb49b5"
  license "MIT"
  head "https://github.com/nburns/chalk.git", branch: "main"

  depends_on "go" => :build

  def install
    ldflags = %W[-s -w -X main.version=#{version}]
    system "go", "build", *std_go_args(ldflags:)
  end

  # brew services uses this block to manage chalk as a launchd/systemd unit.
  # On Linux, `chalk service install` (via kardianos/service) remains available
  # as an alternative for systemd-managed installs.
  service do
    run [opt_bin/"chalk"]
    keep_alive true
    log_path var/"log/chalk.log"
    error_log_path var/"log/chalk.log"
    environment_variables BLACKBOARD_DB:   var/"chalk/board.db",
                          BLACKBOARD_HOST: "127.0.0.1",
                          BLACKBOARD_PORT: "8080"
  end

  test do
    port = free_port
    pid = spawn({ "BLACKBOARD_PORT" => port.to_s }, bin/"chalk", err: "/dev/null")
    sleep 1
    output = shell_output("curl -sf http://127.0.0.1:#{port}/mcp 2>&1 || true")
    assert_match "blackboard", output
  ensure
    Process.kill("TERM", pid)
  end
end
