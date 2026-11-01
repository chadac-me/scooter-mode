;;; scooter-agent-shell.el --- Chat with Scooter through agent-shell  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;;; Commentary:

;; Each server becomes an agent-shell agent that runs `scooter acp'.  The
;; bridge asks this Emacs for auth headers through emacsclient, so login and
;; refresh stay in `scooter-auth'.  In a Scooter shell, `find-file' starts in
;; the conversation's sandbox over Tramp.

;;; Code:

(require 'cl-lib)
(require 'map)
(require 'server)
(require 'scooter-server)
(require 'scooter-auth)
(require 'scooter-install)

(declare-function agent-shell-make-agent-config "agent-shell")
(declare-function agent-shell-start "agent-shell")
(declare-function agent-shell--make-acp-client "agent-shell")
(declare-function scooter-tramp-file-name "scooter-tramp")
(defvar agent-shell-agent-configs)
(defvar agent-shell--state)

(defcustom scooter-acp-command '("scooter" "acp")
  "Command and leading arguments that run the ACP bridge.
If the command isn't on PATH, `scooter-install-binary' may download it."
  :type '(repeat string)
  :group 'scooter)

(defcustom scooter-emacsclient-program
  (or (executable-find "emacsclient") "emacsclient")
  "The emacsclient the bridge calls for auth headers."
  :type 'string
  :group 'scooter)

(defun scooter-agent-shell-headers-command (server)
  "Return the shell command printing SERVER's headers via emacsclient."
  (concat (shell-quote-argument scooter-emacsclient-program)
          " -s " (shell-quote-argument (if server-socket-dir
                                           (expand-file-name server-name server-socket-dir)
                                         server-name))
          ;; Single quotes, not `shell-quote-argument': {retry} must stay literal.
          (format " --eval '(scooter-auth-headers-json \"%s\" {retry})'" server)))

(defun scooter-agent-shell--headers-args (server)
  "Return the bridge's auth arguments for SERVER."
  (if (server-running-p server-name)
      (list "--headers-command" (scooter-agent-shell-headers-command server))
    ;; Without an Emacs server, hand over a snapshot; it can't refresh.
    (let ((file (with-file-modes #o600 (make-temp-file "scooter-acp-h"))))
      (with-temp-file file (insert (scooter-auth-headers-json server)))
      (list "--headers-file" file))))

(cl-defun scooter-acp-arguments (server &key project)
  "Return the bridge arguments for SERVER.
PROJECT non-nil puts the working directory's git checkout in the first prompt."
  (if (scooter-server-get server :self)
      (append (cdr scooter-acp-command) '("--self") (and project '("--project")))
    (append (cdr scooter-acp-command)
            (list "--server" (scooter-server-url server))
            (when-let* ((model (scooter-server-get server :model))) (list "--model" model))
            (and project '("--project"))
            (scooter-agent-shell--headers-args server))))

(cl-defun scooter-agent-shell-config (server &key title project)
  "Return an agent-shell config for SERVER.
TITLE names the buffer; PROJECT is as in `scooter-acp-arguments'."
  (require 'agent-shell)
  (let ((name (if title (format "Scooter %s@%s" title server) (format "Scooter[%s]" server))))
    (agent-shell-make-agent-config
     :identifier (intern (format "scooter-%s" server))
     :mode-line-name (format "Scooter[%s]" server)
     :buffer-name name
     :shell-prompt "Scooter> "
     :shell-prompt-regexp "Scooter> "
     :client-maker
     (lambda (buffer)
       (agent-shell--make-acp-client
        :command (scooter-install-program (car scooter-acp-command))
        :command-params (scooter-acp-arguments server :project project)
        :context-buffer buffer))
     :install-instructions "Install the bridge: nix profile install github:chadac-me/scooter-mode")))

;;;; Sandbox files

(defun scooter-agent-shell--server ()
  "Return the Scooter server of this agent-shell buffer, or nil."
  (when-let* ((state (bound-and-true-p agent-shell--state))
              (id (map-nested-elt state '(:agent-config :identifier)))
              ((string-prefix-p "scooter-" (symbol-name id)))
              (server (intern (string-remove-prefix "scooter-" (symbol-name id))))
              ((memq server (scooter-server-names))))
    server))

(defvar-local scooter-agent-shell--directory nil
  "This shell's sandbox /workspace/ as a Tramp name, once known.")

(defun scooter-agent-shell-conversation ()
  "Return (SERVER ID) for this shell's remote conversation, or nil.
It is nil outside a Scooter shell, for a :self server, whose sandbox is
local, and before the conversation exists."
  (when-let* ((server (scooter-agent-shell--server))
              ((not (scooter-server-get server :self)))
              (id (map-nested-elt agent-shell--state '(:session :id))))
    (list server id)))

(defun scooter-agent-shell-directory ()
  "Return this shell's sandbox /workspace/ over Tramp, or nil."
  (or scooter-agent-shell--directory
      (when-let* ((conv (scooter-agent-shell-conversation)))
        (setq scooter-agent-shell--directory (apply #'scooter-tramp-file-name conv)))))

(defun scooter-agent-shell-find-file ()
  "Run `find-file' from this conversation's sandbox, when there is one."
  (interactive)
  (let ((default-directory (or (scooter-agent-shell-directory) default-directory)))
    (call-interactively #'find-file)))

(defvar-keymap scooter-agent-shell-files-mode-map
  "<remap> <find-file>" #'scooter-agent-shell-find-file)

(define-minor-mode scooter-agent-shell-files-mode
  "Make `find-file' in a Scooter shell start in its sandbox."
  :keymap scooter-agent-shell-files-mode-map)

(defun scooter-agent-shell--enable-files ()
  "Turn on `scooter-agent-shell-files-mode' in Scooter shells."
  (when (scooter-agent-shell--server)
    (scooter-agent-shell-files-mode 1)))

(add-hook 'agent-shell-mode-hook #'scooter-agent-shell--enable-files)

;;;###autoload
(defun scooter-agent-shell-register ()
  "Add one agent-shell agent per entry in `scooter-servers'."
  (interactive)
  (require 'agent-shell)
  (dolist (server (scooter-server-names))
    (let ((maker (lambda () (scooter-agent-shell-config server))))
      (if (functionp agent-shell-agent-configs)
          (let ((base agent-shell-agent-configs))
            (setq agent-shell-agent-configs (lambda () (cons maker (funcall base)))))
        (push maker agent-shell-agent-configs)))))

(provide 'scooter-agent-shell)
;;; scooter-agent-shell.el ends here
