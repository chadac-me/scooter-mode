;;; scooter-tramp.el --- Open sandbox files with Tramp  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;;; Commentary:

;; A Tramp method for a conversation's sandbox:
;;
;;   /scooter:<conversation>@<server>:/workspace/...
;;
;; <conversation> is the title slug from `scooter-list' (or an id prefix).
;; Tramp runs ssh with `scooter tunnel' as its ProxyCommand, which opens the
;; sandbox's `ssh' web service over a WebSocket, waking the sandbox and
;; starting the service first.  Before each connect this Emacs writes the
;; conversation id and current auth headers to a 0600 file the tunnel reads:
;; Tramp blocks Emacs while it connects, so the tunnel can't call back.

;;; Code:

(require 'cl-lib)
(require 'tramp)
(require 'tramp-sh)
(require 'scooter-server)
(require 'scooter-http)
(require 'scooter-auth)
(require 'scooter-list)
(require 'scooter-install)
(require 'scooter-agent-shell)
(require 'scooter)

(defconst scooter-tramp-method "scooter"
  "The Tramp method name for Scooter sandboxes.")

(defcustom scooter-tramp-ssh-program "ssh"
  "The ssh client Tramp runs for Scooter sandboxes."
  :type 'string
  :group 'scooter)

(defcustom scooter-tramp-ssh-keygen-program "ssh-keygen"
  "The ssh-keygen that makes the client key."
  :type 'string
  :group 'scooter)

(defcustom scooter-tramp-directory (locate-user-emacs-file "scooter/ssh/")
  "Where the client key, known hosts and tunnel specs live."
  :type 'directory
  :group 'scooter)

(defun scooter-tramp--file (name)
  "Return NAME under `scooter-tramp-directory'."
  (expand-file-name name scooter-tramp-directory))

(defun scooter-tramp--ensure-directory ()
  "Make `scooter-tramp-directory', private to this user."
  (unless (file-directory-p scooter-tramp-directory)
    (with-file-modes #o700 (make-directory scooter-tramp-directory t))))

(defun scooter-tramp--ensure-key ()
  "Make the client key unless it exists.
Any key works: the proxy's auth already gates the sandbox."
  (let ((key (scooter-tramp--file "id_ed25519")))
    (unless (file-exists-p key)
      (unless (zerop (call-process scooter-tramp-ssh-keygen-program nil nil nil
                                   "-q" "-t" "ed25519" "-N" "" "-C" "scooter-mode" "-f" key))
        (user-error "Scooter: %s failed to make %s" scooter-tramp-ssh-keygen-program key)))
    key))

(defun scooter-tramp--conversations (server)
  "Return SERVER's top-level conversations, most recent first."
  (cl-sort (seq-remove (lambda (c) (alist-get 'parentId c))
                       (append (scooter-request server "GET" "/conversations") nil))
           #'> :key (lambda (c) (or (alist-get 'lastActivityAt c) 0))))

(defun scooter-tramp-resolve (server name)
  "Return the id of SERVER's conversation NAME: a slug or an id prefix.
A slug shared by several conversations names the most recent."
  (let ((convs (scooter-tramp--conversations server)))
    (alist-get 'id (or (cl-find-if (lambda (c) (equal (scooter-slug (alist-get 'title c) (alist-get 'id c))
                                                      name))
                                   convs)
                       (cl-find-if (lambda (c) (string-prefix-p name (alist-get 'id c))) convs)
                       (user-error "Scooter[%s]: no conversation named %s" server name)))))

(defun scooter-tramp-spec-json (server name)
  "Return the tunnel spec for SERVER's conversation NAME as JSON."
  (when (scooter-server-api-key-p server)
    (user-error "Scooter[%s]: Tramp needs a cookie or OIDC login, not an API key" server))
  (let ((json-encoding-pretty-print nil))
    (json-encode `((url . ,(scooter-server-url server))
                   (conversation . ,(scooter-tramp-resolve server name))
                   (headers . ,(or (scooter-auth-headers server) (make-hash-table)))))))

(defun scooter-tramp--write-spec (server name)
  "Write the spec for SERVER's conversation NAME where the tunnel reads it."
  (let ((json (scooter-tramp-spec-json (intern server) name)))
    (with-file-modes #o600
      (with-temp-file (scooter-tramp--file (format "%s.%s.json" name server))
        (insert json)))))

(defun scooter-tramp-login-args ()
  "Return the method's `tramp-login-args'.
Tramp joins them into a shell command, so each is shell-quoted here, and
fills in %u (conversation) and %h (server)."
  (let ((proxy (concat (shell-quote-argument (scooter-install-program (car scooter-acp-command)))
                       " tunnel --spec-file "
                       (shell-quote-argument (scooter-tramp--file "%u.%h.json")))))
    `(("-F" "none")
      ("-l" "root")
      ("-o" ,(shell-quote-argument (concat "ProxyCommand=" proxy)))
      ("-i" ,(shell-quote-argument (scooter-tramp--file "id_ed25519")))
      ("-o" "IdentitiesOnly=yes")
      ("-o" ,(shell-quote-argument (concat "UserKnownHostsFile=" (scooter-tramp--file "known_hosts"))))
      ;; The host key persists on the workspace volume.
      ("-o" "StrictHostKeyChecking=accept-new")
      ;; Gives each conversation its own ControlPath.
      ("-o" "HostName=%u.%h")
      ("%c") ("%w") ("-e" "none") ("%h"))))

(defun scooter-tramp--prepare (vec &rest _)
  "Before Tramp connects VEC, make the key and write the tunnel spec."
  (when (and (equal (tramp-file-name-method vec) scooter-tramp-method)
             (not (process-live-p (tramp-get-connection-process vec))))
    (scooter-tramp--ensure-directory)
    (scooter-tramp--ensure-key)
    (scooter-tramp--write-spec (tramp-file-name-host vec) (tramp-file-name-user vec))
    ;; Recomputed here so a newly installed binary takes effect.
    (setf (alist-get 'tramp-login-args (cdr (assoc scooter-tramp-method tramp-methods)))
          (list (scooter-tramp-login-args)))))

(defun scooter-tramp-completions (_file)
  "Return (CONVERSATION SERVER) pairs for completing /scooter: names."
  (cl-loop for server in (scooter-server-names)
           unless (scooter-server-get server :self)
           nconc (mapcar (lambda (c) (list (scooter-slug (alist-get 'title c) (alist-get 'id c))
                                           (symbol-name server)))
                         (ignore-errors (scooter-tramp--conversations server)))))

;;;###autoload
(defun scooter-tramp-enable ()
  "Add the /scooter: Tramp method."
  (setf (alist-get scooter-tramp-method tramp-methods nil nil #'equal)
        `((tramp-login-program ,scooter-tramp-ssh-program)
          ;; Filled in by `scooter-tramp--prepare' on connect.
          (tramp-login-args (("%h")))
          (tramp-async-args (("-q")))
          (tramp-direct-async ("-t" "-t"))
          (tramp-remote-shell ,tramp-default-remote-shell)
          (tramp-remote-shell-login ("-l"))
          (tramp-remote-shell-args ("-c"))))
  (tramp-set-completion-function scooter-tramp-method '((scooter-tramp-completions "")))
  (advice-add 'tramp-maybe-open-connection :before #'scooter-tramp--prepare))

;;;###autoload (with-eval-after-load 'tramp (scooter-tramp-enable))

;;;###autoload
(defun scooter-tramp-file-name (server id &optional path)
  "Return the Tramp name of PATH (default /workspace/) in SERVER's conversation ID.
It uses the slug when that names ID, else the id itself."
  (let ((slug (scooter--title-of server id)))
    (format "/%s:%s@%s:%s" scooter-tramp-method
            (if (and slug (equal (ignore-errors (scooter-tramp-resolve server slug)) id)) slug id)
            server (or path "/workspace/"))))

;;;###autoload
(defun scooter-find-file (server id)
  "Open /workspace in SERVER's conversation ID over Tramp.
In a Scooter shell it opens that shell's conversation; a prefix argument asks."
  (interactive (or (and (not current-prefix-arg) (scooter-agent-shell-conversation))
                   (let ((server (scooter-current-server current-prefix-arg)))
                     (list server (scooter-read-conversation server)))))
  (find-file (if (equal (list server id) (scooter-agent-shell-conversation))
                 (scooter-agent-shell-directory)
               (scooter-tramp-file-name server id))))

(provide 'scooter-tramp)
;;; scooter-tramp.el ends here
