# Homebrew formula stub for aforo-loadgen.
#
# This file is consumed by the public tap at github.com/aforoai/homebrew-tap.
# Session 9 wires real release artifacts; until then `brew install` from the
# tap will fail by design — install via `go install` for now.
class Loadgen < Formula
  desc "Load-test CLI for the Aforo NextGen ingestion pipeline"
  homepage "https://github.com/aforoai/aforo-nextgen-loadgen"
  license "Apache-2.0"
  version "0.0.0-dev"

  # Real darwin-arm64/darwin-amd64/linux-arm64/linux-amd64 binaries get wired
  # in Session 9. Until then this stanza intentionally fails so we don't ship
  # a broken brew install path.
  on_macos do
    on_arm do
      url "https://example.invalid/aforo-loadgen-darwin-arm64.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
  end

  def install
    bin.install "aforo-loadgen"
  end

  test do
    assert_match "aforo-loadgen", shell_output("#{bin}/aforo-loadgen version")
  end
end
