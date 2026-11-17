# scooter-mode task runner: `just` with no args lists recipes.

default:
    @just --list

# Evaluate the flake structure (fast; does not build derivations).
check-flake:
    nix flake show

# Byte-compile the Elisp, warnings as errors.
compile:
    emacs -Q --batch -L lisp --eval '(setq byte-compile-error-on-warn t)' -f batch-byte-compile lisp/*.el
    rm -f lisp/*.elc

# Each file loads (or autoloads) every function it calls.
deps:
    ok=0; for f in lisp/*.el; do emacs -Q --batch -L lisp -l test/check-deps.el "$f" || ok=1; done; exit $ok

# ERT tests with the dev shell's pinned nixpkgs dependencies.
ert:
    emacs -Q --batch -L lisp -L test -l test/scooter-test.el -f ert-run-tests-batch-and-exit

# ERT tests via Cask against the latest MELPA dependencies (needs network).
ert-melpa:
    cask install
    cask exec ert-runner

# All unit tests.
test-unit: deps ert

# Fast checks, as CI runs them.
ci: check-flake compile test-unit
    @echo "✅ ci passed"

