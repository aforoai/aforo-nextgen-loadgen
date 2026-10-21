#!/usr/bin/env bash
# Verifies a loadgen-minted service token with aforo-common's real Java
# ServiceTokenVerifier (G375 cross-check). Compiles the serviceauth sources
# from an aforo-common checkout against jars already in ~/.m2 — no network,
# no mvn install.
#
#   AFORO_COMMON_DIR=../aforo-nextgen-common scripts/serviceauth-java-crosscheck/run.sh \
#       <token> <audience> <kid> <publicKey> <nowEpochSeconds>
#
# The Go test TestJavaVerifierCrossCheck calls this when AFORO_COMMON_DIR is set.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
common="${AFORO_COMMON_DIR:?set AFORO_COMMON_DIR to an aforo-nextgen-common checkout}"
m2="${M2_REPO:-$HOME/.m2/repository}"
src="$common/src/main/java/com/aforo/billing/common/serviceauth"
jar() { local f="$m2/$1"; [[ -f "$f" ]] || { echo "missing $f" >&2; exit 3; }; printf '%s' "$f"; }
cp="$(jar io/jsonwebtoken/jjwt-api/0.12.5/jjwt-api-0.12.5.jar)"
cp+=":$(jar io/jsonwebtoken/jjwt-impl/0.12.5/jjwt-impl-0.12.5.jar)"
cp+=":$(jar io/jsonwebtoken/jjwt-jackson/0.12.5/jjwt-jackson-0.12.5.jar)"
cp+=":$(jar com/fasterxml/jackson/core/jackson-databind/2.17.0/jackson-databind-2.17.0.jar)"
cp+=":$(jar com/fasterxml/jackson/core/jackson-core/2.17.0/jackson-core-2.17.0.jar)"
cp+=":$(jar com/fasterxml/jackson/core/jackson-annotations/2.17.0/jackson-annotations-2.17.0.jar)"
cp+=":$(jar org/slf4j/slf4j-api/2.0.7/slf4j-api-2.0.7.jar)"
cp+=":$(jar org/springframework/boot/spring-boot/3.2.5/spring-boot-3.2.5.jar)"
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT
javac -nowarn -d "$out" -cp "$cp" \
  "$src/ServiceTokenVerifier.java" "$src/ServiceTokenException.java" "$src/ServiceTokens.java" \
  "$src/ServiceKeys.java" "$src/ServiceAuthKeyMaterial.java" "$src/ServiceAuthProperties.java" \
  "$src/VerifiedServiceCall.java" "$src/ServiceAuthMode.java" "$here/VerifyServiceToken.java" >"$out/javac.log" 2>&1 \
  || { cat "$out/javac.log" >&2; exit 3; }
exec java -cp "$out:$cp" VerifyServiceToken "$@"
