# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities **privately**, through GitHub's Private
Vulnerability Reporting:

> [Report a vulnerability](https://github.com/cboxdk/fpm-tune/security/advisories/new)
> — the "Report a vulnerability" button under the repository's **Security** tab.

That opens a private advisory only the maintainers can see. Please do not open a
public issue for a security problem before it has been addressed.

This is a young project maintained on a best-effort basis. There is no PGP key,
no security mailbox, and no guaranteed response time — the honest position is
that reports are read and acted on as soon as they can be, not against an SLA we
would be inventing here.

## What is in scope

fpm-tune writes production PHP-FPM configuration and reloads a live master, so
the interesting reports are the ones about that power being misdirected. In
particular:

- **Execution of an untrusted binary or config.** Discovery hands a php-fpm
  binary and config path to `php-fpm -t` / a reload. Both are checked for
  ownership before exec (see `phpfpm`'s `trustedPath`); a way past that check is
  in scope.
- **Writing where it should not.** It writes only `pm.*` keys into a pool
  directory it was pointed at, validates with `php-fpm -t` before anything is
  moved into place, and rolls back a rejected drop-in. A path that gets it to
  write outside that directory, skip validation, or leave a host in a broken
  state is in scope.
- **Sizing from a budget it could not read.** It refuses to write when the
  memory limit could not be *read* (distinct from "no limit"). A case where it
  writes anyway, from a limit it did not actually establish, is in scope.
- **Reload that restarts.** It signals `SIGUSR2` and must never restart the
  master. A path that kills workers or the master is in scope.

## What is not

- Running the tool against a host you do not control, or pointing `--drop-in-dir`
  at a directory you should not write to. It does what it is told by the operator
  who runs it; that is not a vulnerability.
- Denial of service from a deliberately hostile local root. The process table is
  not treated as a trust boundary, but a local root can already reconfigure
  php-fpm directly.

## Supported versions

During the beta, only the latest tagged release is supported. Please reproduce
against it before reporting.
