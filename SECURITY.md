# Security Policy

## Reporting a Vulnerability

If you believe you have found a security vulnerability in SoundBoard, please report it privately. **Do not open a public issue.**

### How to report

- **Preferred:** Use GitHub's private vulnerability reporting at
  `https://github.com/jodagreyhame/SoundBoard/security/advisories/new`.
There is no security email address for this project; please use GitHub's private reporting so the
report stays confidential until a fix ships.

Include, if you can:

- A description of the issue and its impact
- Steps to reproduce (or a proof-of-concept)
- The version or commit hash you tested against
- Any suggested mitigation

## What to expect

- **Acknowledgement:** within 2 days of your report.
- **Triage & assessment:** within 7 days we will confirm the vulnerability, assess severity, and respond with a plan.
- **Fix:** critical/high-severity issues are targeted for patch release within 30 days of confirmation. Lower-severity issues are scheduled for the next regular release.
- **Disclosure:** we prefer coordinated disclosure. After a fix is released, we will publish a security advisory crediting the reporter (unless you prefer to remain anonymous).

## Supported versions

| Version | Supported |
|---|---|
| 1.x | ✅ |
| older | ❌ |

Only the latest major version receives security fixes. Users on older versions should upgrade.

## Scope

In scope:

- The SoundBoard binary/library itself
- First-party dependencies maintained by the SoundBoard team
- Documented configuration interfaces

Out of scope:

- Third-party dependencies (report to the upstream project; we will coordinate if impact is severe)
- Social-engineering attacks on maintainers
- Denial-of-service that requires no authentication bypass (rate-limit issues, etc.)

## Safe harbor

We will not pursue legal action against researchers who:

- Test only against their own installations or our designated testing environment
- Respect user privacy and do not access data beyond what is necessary to demonstrate the vulnerability
- Report in good faith and give us reasonable time to respond before public disclosure
- Do not exploit the vulnerability beyond proof-of-concept

## Hall of Fame

Security researchers who have responsibly disclosed vulnerabilities are credited in [SECURITY_CREDITS.md](./SECURITY_CREDITS.md) unless they request otherwise.

## Security-relevant surface

Two areas warrant particular attention from reviewers and reporters:

- **Elevated third-party driver install.** SoundBoard can download the official VB-CABLE
  installer from VB-Audio's CDN and run it elevated at the user's request
  (`internal/setup/install.go`). The download is HTTPS host-pinned, rejects redirects, and is
  zip-slip guarded, but this is the highest-privilege operation the app performs.
- **Default audio device hijack.** While running, SoundBoard sets the Windows default recording
  device to the virtual cable via the undocumented `IPolicyConfig` COM interface, and restores
  the previous device on exit (`internal/winaudio/`).

