;;; scooter-agent-shell.el --- Chat with Scooter through agent-shell  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;;; Commentary:

;; Each server becomes an agent-shell agent that runs `scooter acp'.  The
;; bridge asks this Emacs for auth headers through emacsclient, so login and
;; refresh stay in `scooter-auth'.  In a Scooter shell, `find-file' starts in
;; the conversation's sandbox over Tramp, and /workspace paths in the shell
;; open the sandbox file.

;;; Code:

(require 'cl-lib)
(require 'map)
(require 'server)
(require 'scooter-server)
(require 'scooter-auth)
(require 'scooter-http)
(require 'scooter-install)
(require 'scooter-list)

(declare-function agent-shell-make-agent-config "agent-shell")
(declare-function agent-shell-start "agent-shell")
(declare-function agent-shell--make-acp-client "agent-shell")
(declare-function agent-shell--format-buffer-name "agent-shell")
(declare-function agent-shell--project-name "agent-shell")
(declare-function agent-shell-subscribe-to "agent-shell")
(declare-function scooter-tramp-file-name "scooter-tramp")
(defvar agent-shell-agent-configs)
(defvar agent-shell--state)
(defvar agent-shell-elicitation--experimental-feature-enabled)

(defcustom scooter-acp-command '("scooter" "acp")
  "Command and leading arguments that run the ACP bridge.
If the command isn't on PATH, `scooter-install-binary' may download it."
  :type '(repeat string)
  :group 'scooter)

(defcustom scooter-agent-shell-forms t
  "Non-nil lets Scooter shells answer the agent's questions in a form.
This turns on agent-shell's experimental elicitation forms in those
buffers only.  With it off, a question points at the web UI instead."
  :type 'boolean
  :group 'scooter)

(defcustom scooter-acp-replay-turns nil
  "When a number N, opening a conversation replays only its last N turns.
Nil replays all of it."
  :type '(choice (const :tag "All" nil) natnum)
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
      (append (cdr scooter-acp-command) '("--self") (and project '("--project"))
              (scooter-agent-shell--replay-args))
    (append (cdr scooter-acp-command)
            (list "--server" (scooter-server-url server))
            (when-let* ((model (scooter-server-get server :model))) (list "--model" model))
            (and project '("--project"))
            (scooter-agent-shell--replay-args)
            (scooter-agent-shell--headers-args server))))

(defun scooter-agent-shell--replay-args ()
  "Return the bridge's replay arguments, from `scooter-acp-replay-turns'."
  (and scooter-acp-replay-turns
       (list "--replay-turns" (number-to-string scooter-acp-replay-turns))))

(defun scooter-agent-shell-name (server &optional title)
  "Return the agent name for SERVER's shell, showing TITLE when given."
  (if title (format "Scooter %s@%s" title server) (format "Scooter[%s]" server)))

(cl-defun scooter-agent-shell-config (server &key title project)
  "Return an agent-shell config for SERVER.
TITLE names the buffer; PROJECT is as in `scooter-acp-arguments'."
  (require 'agent-shell)
  (let ((name (scooter-agent-shell-name server title)))
    (agent-shell-make-agent-config
     :identifier (intern (format "scooter-%s" server))
     :mode-line-name (format "Scooter[%s]" server)
     :buffer-name name
     :shell-prompt "Scooter> "
     :shell-prompt-regexp "Scooter> "
     :client-maker
     (lambda (buffer)
       (when scooter-agent-shell-forms
         ;; Read when agent-shell sends initialize, after this runs.
         (with-current-buffer buffer
           (setq-local agent-shell-elicitation--experimental-feature-enabled t)))
       (agent-shell--make-acp-client
        :command (scooter-install-program (car scooter-acp-command))
        :command-params (scooter-acp-arguments server :project project)
        :context-buffer buffer))
     :install-instructions "Install the bridge: nix profile install github:chadac-me/scooter-mode")))

;;;; Buffer names

(defun scooter-agent-shell-retitle (&optional _event)
  "Rename this shell after its conversation's title slug.
A buffer the user renamed keeps its name."
  (when-let* ((conv (scooter-agent-shell-conversation))
              (reply (ignore-errors
                       (scooter-request (car conv) "GET" (format "/conversations/%s" (cadr conv)))))
              (title (alist-get 'title reply))
              (old (map-nested-elt agent-shell--state '(:agent-config :buffer-name)))
              (new (scooter-agent-shell-name (car conv) (scooter-slug title)))
              ((not (equal old new)))
              (project (agent-shell--project-name))
              ((string-prefix-p (agent-shell--format-buffer-name old project) (buffer-name))))
    ;; Shadow rather than mutate: the config may be shared.
    (setf (map-elt agent-shell--state :agent-config)
          (cons (cons :buffer-name new) (map-elt agent-shell--state :agent-config)))
    (rename-buffer (agent-shell--format-buffer-name new project) t)))

(defun scooter-agent-shell--watch-title ()
  "Retitle Scooter shells after each turn."
  (when (scooter-agent-shell--server)
    (agent-shell-subscribe-to :shell-buffer (current-buffer) :event 'turn-complete
                              :on-event (lambda (_) (scooter-agent-shell-retitle)))))

(add-hook 'agent-shell-mode-hook #'scooter-agent-shell--watch-title)

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

(defun scooter-agent-shell-dired ()
  "Open Dired on this conversation's sandbox /workspace/."
  (interactive)
  (dired (or (scooter-agent-shell-directory)
             (user-error "Scooter: this shell has no remote sandbox yet"))))

;;;; Sandbox paths in the shell

(defcustom scooter-agent-shell-link-paths t
  "Non-nil makes /workspace paths in Scooter shells open the sandbox file.
A path may end in :LINE, :LINE:COLUMN or #LLINE."
  :type 'boolean
  :group 'scooter)

(defconst scooter-agent-shell--path-chars "[:alnum:]._~/@+-"
  "The chars of a linked path, as in `skip-chars-forward'.")

(defconst scooter-agent-shell--path-regexp
  (rx "/workspace" (? "/" (* (any alnum "._~/@+-")))
      (? (or ":" "#L") (group (+ digit)) (? ":" (group (+ digit)))))
  "A sandbox path, with the line and column it may name.")

(defun scooter-agent-shell-sandbox-file (path)
  "Return PATH, a path in this shell's sandbox, as a file name here.
In a remote conversation that is its Tramp name; else PATH itself."
  (if-let* ((dir (scooter-agent-shell-directory)))
      (concat (file-remote-p dir) path)
    path))

(defun scooter-agent-shell-open-path (path &optional line column)
  "Visit PATH in this shell's sandbox, at LINE and COLUMN when given."
  (let ((file (scooter-agent-shell-sandbox-file path)))
    (if (fboundp 'agent-shell-markdown-visit-file)
        (agent-shell-markdown-visit-file :file file :line-start line :column column)
      (find-file file)
      (when line
        (goto-char (point-min))
        (forward-line (1- line))
        (when column (move-to-column (1- column)))))))

(defun scooter-agent-shell-open-path-at-point (&optional event)
  "Visit the sandbox path at point, or where EVENT clicked."
  (interactive (list last-nonmenu-event))
  (let* ((pos (if (mouse-event-p event) (posn-point (event-end event)) (point)))
         (buffer (if (mouse-event-p event) (window-buffer (posn-window (event-end event))) (current-buffer)))
         (target (with-current-buffer buffer
                   (seq-some (lambda (ov) (overlay-get ov 'scooter-path)) (overlays-at pos)))))
    (unless target (user-error "No sandbox path here"))
    (with-current-buffer buffer
      (apply #'scooter-agent-shell-open-path target))))

(defvar-keymap scooter-agent-shell-path-map
  :doc "Keys on a sandbox path in a Scooter shell."
  "RET" #'scooter-agent-shell-open-path-at-point
  "<mouse-2>" #'scooter-agent-shell-open-path-at-point
  "<follow-link>" 'mouse-face)

(defun scooter-agent-shell--path-char-p (char)
  "Non-nil when CHAR can be part of a linked path."
  (and char (string-match-p (concat "[" scooter-agent-shell--path-chars "]") (string char))))

(defun scooter-agent-shell--link-paths (beg end)
  "Link the sandbox paths in the lines around BEG..END."
  (save-excursion
    (save-match-data
      (let ((beg (max (progn (goto-char beg) (line-beginning-position)) (- beg 512) (point-min)))
            (end (min (progn (goto-char end) (line-end-position)) (+ end 512) (point-max)))
            (case-fold-search nil))
        ;; Relink whole old links, which the edit may have cut short.
        (dolist (ov (overlays-in beg end))
          (when (overlay-get ov 'scooter-path)
            (setq beg (min beg (overlay-start ov))
                  end (max end (overlay-end ov)))
            (delete-overlay ov)))
        (goto-char beg)
        (while (re-search-forward scooter-agent-shell--path-regexp end t)
          (let* ((start (match-beginning 0))
                 (line (and (match-beginning 1) (string-to-number (match-string 1))))
                 (column (and (match-beginning 2) (string-to-number (match-string 2))))
                 (path-end (if line
                               ;; Before the ":" or "#L".
                               (- (match-beginning 1)
                                  (if (eq (char-before (match-beginning 1)) ?:) 1 2))
                             (match-end 0)))
                 ;; A sentence's full stop isn't part of the path.
                 (path (string-trim-right (buffer-substring-no-properties start path-end) "[.]+"))
                 (link-end (if line (match-end 0) (+ start (length path)))))
            (unless (or (scooter-agent-shell--path-char-p (char-before start))
                        ;; /workspacefoo, not /workspace.
                        (and (equal path "/workspace")
                             (scooter-agent-shell--path-char-p (char-after (+ start (length path)))))
                        ;; A path running into a word: /workspace/a:12b.
                        (and line (scooter-agent-shell--path-char-p (char-after link-end))
                             (not (eq (char-after link-end) ?.))))
              (let ((ov (make-overlay start link-end nil t nil)))
                (overlay-put ov 'scooter-path (list path line column))
                (overlay-put ov 'face 'link)
                (overlay-put ov 'mouse-face 'highlight)
                (overlay-put ov 'help-echo "RET, mouse-1: open in the Scooter sandbox")
                (overlay-put ov 'keymap scooter-agent-shell-path-map)
                (overlay-put ov 'evaporate t)))))))))

(defun scooter-agent-shell--link-changed (beg end _old-length)
  "Link sandbox paths in text changed between BEG and END."
  (scooter-agent-shell--link-paths beg end))

(defun scooter-agent-shell--unlink-paths ()
  "Remove this buffer's sandbox path links."
  (dolist (ov (overlays-in (point-min) (point-max)))
    (when (overlay-get ov 'scooter-path) (delete-overlay ov))))

(defvar-keymap scooter-agent-shell-files-mode-map
  :doc "Keys for the sandbox's files in a Scooter shell.
Rebind them here, e.g. (keymap-set scooter-agent-shell-files-mode-map
\"C-c f\" #\\='scooter-agent-shell-find-file)."
  "<remap> <find-file>" #'scooter-agent-shell-find-file
  "C-c C-f" #'scooter-agent-shell-find-file
  ;; C-c C-d is comint's send-EOF; C-c C-j echoes C-x C-j, `dired-jump'.
  "C-c C-j" #'scooter-agent-shell-dired)

(define-minor-mode scooter-agent-shell-files-mode
  "Open a Scooter shell's sandbox files.
`find-file' starts in the sandbox, and /workspace paths in the shell
\(with `scooter-agent-shell-link-paths') open the sandbox file, at the
line they name.

\\{scooter-agent-shell-files-mode-map}"
  :keymap scooter-agent-shell-files-mode-map
  (scooter-agent-shell--unlink-paths)
  (remove-hook 'after-change-functions #'scooter-agent-shell--link-changed t)
  (when (and scooter-agent-shell-files-mode scooter-agent-shell-link-paths)
    (add-hook 'after-change-functions #'scooter-agent-shell--link-changed nil t)
    (scooter-agent-shell--link-paths (point-min) (point-max))))

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
