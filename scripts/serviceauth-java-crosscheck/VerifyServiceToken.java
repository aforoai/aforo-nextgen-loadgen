import com.aforo.billing.common.serviceauth.ServiceAuthKeyMaterial;
import com.aforo.billing.common.serviceauth.ServiceAuthProperties;
import com.aforo.billing.common.serviceauth.ServiceTokenException;
import com.aforo.billing.common.serviceauth.ServiceTokenVerifier;
import com.aforo.billing.common.serviceauth.VerifiedServiceCall;

import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.List;
import java.util.Map;

/**
 * Verifies one service token with aforo-common's own ServiceTokenVerifier, configured exactly as
 * a target that trusts aforo-loadgen would be (G375 cross-check; see run.sh).
 *
 * <p>args: token audience kid publicKey nowEpochSeconds. Prints "OK iss=.. tenant=.. scopes=..
 * jti=.." or "REJECTED <reason>". Exit 0 on OK, 1 on rejection, 2 on bad usage.
 */
public final class VerifyServiceToken {
    public static void main(String[] args) {
        if (args.length != 5) {
            System.err.println("usage: VerifyServiceToken <token> <audience> <kid> <publicKey> <nowEpochSeconds>");
            System.exit(2);
        }
        ServiceAuthProperties props = new ServiceAuthProperties();
        props.setTrusted(Map.of("aforo-loadgen", List.of(new ServiceAuthProperties.TrustedKey(args[2], args[3]))));
        ServiceAuthKeyMaterial keys = ServiceAuthKeyMaterial.resolve(props, args[1]);
        Clock clock = Clock.fixed(Instant.ofEpochSecond(Long.parseLong(args[4])), ZoneOffset.UTC);
        try {
            VerifiedServiceCall call = new ServiceTokenVerifier(keys, props, clock).verify(args[0]);
            System.out.println("OK iss=" + call.caller() + " tenant=" + call.tenantId()
                    + " scopes=" + String.join(",", new java.util.TreeSet<>(call.scopes())) + " jti=" + call.tokenId());
        } catch (ServiceTokenException e) {
            System.out.println("REJECTED " + e.reason());
            System.exit(1);
        }
    }
}
