;;; scooter-install.el --- Fetch the scooter bridge binary  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;;; Commentary:

;; When `scooter' isn't on PATH, download this package's release of the
;; bridge from GitHub, check it against the release's SHA256SUMS, and keep it
;; under `user-emacs-directory'.

;;; Code:

(require 'url)
(require 'url-http)

(defvar url-http-end-of-headers)
(defvar url-http-response-status)

(defconst scooter-install-version "0.1.0"
  "The bridge release this package expects; matches the package version.")

(defcustom scooter-install-binary 'ask
  "Whether to download the bridge when it isn't on PATH.
`ask' prompts first, t downloads without asking, nil never downloads."
  :type '(choice (const :tag "Ask" ask) (const :tag "Always" t) (const :tag "Never" nil))
  :group 'scooter)

(defcustom scooter-install-release-url
  "https://github.com/chadac-me/scooter-mode/releases/download/v%s/"
  "Release download URL; %s is the version."
  :type 'string
  :group 'scooter)

(defun scooter-install--platform ()
  "Return the release asset suffix for this machine, like \"linux-amd64\"."
  (let ((os (pcase system-type
              ('gnu/linux "linux")
              ('darwin "darwin")
              (other (user-error "Scooter: no bridge release for %s" other))))
        (arch (cond ((string-match-p "\\`\\(x86_64\\|amd64\\)" system-configuration) "amd64")
                    ((string-match-p "\\`\\(aarch64\\|arm64\\)" system-configuration) "arm64")
                    (t (user-error "Scooter: no bridge release for %s" system-configuration)))))
    (concat os "-" arch)))

(defun scooter-install-path (&optional version)
  "Where the downloaded bridge for VERSION lives."
  (locate-user-emacs-file
   (format "scooter/bin/scooter-%s" (or version scooter-install-version))))

(defun scooter-install--fetch (url)
  "Return URL's body as a unibyte string, signalling on HTTP errors."
  (let ((buf (url-retrieve-synchronously url t t 60)))
    (unless buf (error "Scooter: no response from %s" url))
    (unwind-protect
        (with-current-buffer buf
          (let ((status (bound-and-true-p url-http-response-status)))
            (unless (eq status 200)
              (error "Scooter: %s returned %s" url status)))
          (set-buffer-multibyte nil)
          (buffer-substring-no-properties (1+ url-http-end-of-headers) (point-max)))
      (kill-buffer buf))))

(defun scooter-install--expected-sum (sums asset)
  "Return ASSET's sha256 from the SHA256SUMS text SUMS."
  (when (string-match (format "^\\([0-9a-f]\\{64\\}\\)[ *]+%s$" (regexp-quote asset)) sums)
    (match-string 1 sums)))

(defun scooter-install (&optional version)
  "Download and verify the bridge for VERSION; return its path."
  (interactive)
  (let* ((version (or version scooter-install-version))
         (base (format scooter-install-release-url version))
         (asset (concat "scooter-" (scooter-install--platform)))
         (want (or (scooter-install--expected-sum
                    (decode-coding-string (scooter-install--fetch (concat base "SHA256SUMS")) 'utf-8)
                    asset)
                   (error "Scooter: %s isn't in v%s's SHA256SUMS" asset version)))
         (bin (scooter-install--fetch (concat base asset)))
         (got (secure-hash 'sha256 bin))
         (path (scooter-install-path version)))
    (unless (equal got want)
      (error "Scooter: %s checksum mismatch (got %s, want %s)" asset got want))
    (make-directory (file-name-directory path) t)
    ;; Write beside the target, then rename, so a crash leaves no half binary.
    (let ((tmp (make-temp-file (concat path "."))))
      (let ((coding-system-for-write 'no-conversion))
        (with-temp-file tmp (set-buffer-multibyte nil) (insert bin)))
      (set-file-modes tmp #o755)
      (rename-file tmp path t))
    (message "Scooter: installed bridge v%s at %s" version path)
    path))

(defun scooter-install-program (program)
  "Return PROGRAM's path, downloading the bridge if it's missing and allowed."
  (or (executable-find program)
      (let ((path (scooter-install-path)))
        (and (file-executable-p path) path))
      (and (if (eq scooter-install-binary 'ask)
               (y-or-n-p (format "Scooter: `%s' isn't on PATH; download bridge v%s from GitHub? "
                                 program scooter-install-version))
             scooter-install-binary)
           (scooter-install))
      (user-error "Scooter: `%s' not found; install it with nix profile install github:chadac-me/scooter-mode"
                  program)))

(provide 'scooter-install)
;;; scooter-install.el ends here
