;;; scooter.el --- Use Scooter from Emacs  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;; Author: Chad Crawford
;; URL: https://github.com/chadac-me/scooter-mode
;; Version: 0.1.0
;; Package-Requires: ((emacs "29.1") (magit-section "4.0") (transient "0.5") (agent-shell "0.1"))
;; Keywords: tools, ai

;;; Commentary:

;; Browse and start Scooter conversations on several servers, and chat with
;; them through agent-shell.  Bind `scooter-command-map' to a prefix, e.g.
;;
;;   (keymap-global-set "C-c s" scooter-command-map)

;;; Code:

(require 'cl-lib)
(require 'project)
(require 'transient)
(require 'scooter-server)
(require 'scooter-http)
(require 'scooter-auth)
(require 'scooter-list)
(require 'scooter-agent-shell)
(declare-function scooter-find-file "scooter-tramp")
(declare-function scooter-sandbox "scooter-conversation")
(declare-function scooter-web-services "scooter-conversation")
(declare-function scooter-links "scooter-conversation")

(declare-function agent-shell-start "agent-shell")

(defun scooter--title-of (server id)
  "Return the slug of SERVER's conversation ID, or nil."
  (ignore-errors
    (let ((conv (scooter-request server "GET" (format "/conversations/%s" id))))
      (scooter-slug (alist-get 'title conv) id))))

;;;###autoload
(defun scooter-open (server id)
  "Open SERVER's conversation ID in agent-shell."
  (interactive (let ((server (scooter-current-server current-prefix-arg)))
                 (list server (scooter-read-conversation server))))
  (agent-shell-start :config (scooter-agent-shell-config
                              server :title (or (scooter--title-of server id) id))
                     :session-id id))

(defun scooter-read-conversation (server)
  "Read one of SERVER's conversations, returning its id."
  (let* ((convs (scooter-request server "GET" "/conversations"))
         (table (mapcar (lambda (c) (cons (scooter-slug (alist-get 'title c) (alist-get 'id c))
                                          (alist-get 'id c)))
                        convs)))
    (cdr (assoc (completing-read (format "Conversation on %s: " server) table nil t) table))))

;;;###autoload
(defun scooter-new (server text &optional model)
  "Start a conversation on SERVER with TEXT, using MODEL, and open it."
  (interactive (let ((server (scooter-current-server current-prefix-arg)))
                 (list server (read-string "Message: ")
                       (and current-prefix-arg (scooter-read-model server)))))
  (let* ((model (or model (scooter-server-get server :model)))
         (conv (scooter-request server "POST" "/conversations" (and model `((model . ,model)))))
         (id (alist-get 'id conv)))
    (scooter-request server "POST" (format "/conversations/%s/messages" id) `((text . ,text)))
    (scooter-open server id)
    id))

(defun scooter-read-model (server)
  "Read a model from SERVER's /models."
  (let ((models (scooter-request server "GET" "/models")))
    (completing-read "Model: " (alist-get 'available models) nil t nil nil
                     (alist-get 'default models))))

(defun scooter--region-quote ()
  "Return the active region quoted with its file and lines."
  (unless (use-region-p) (user-error "No region"))
  (let ((beg (region-beginning)) (end (region-end)))
    (format "%s (lines %d-%d):\n```\n%s\n```"
            (or (and buffer-file-name (abbreviate-file-name buffer-file-name)) (buffer-name))
            (line-number-at-pos beg) (line-number-at-pos (max beg (1- end)))
            (buffer-substring-no-properties beg end))))

;;;###autoload
(defun scooter-new-from-region (server text)
  "Start a conversation on SERVER about the region, asking TEXT about it."
  (interactive (list (scooter-current-server current-prefix-arg) (read-string "About the region: ")))
  (scooter-new server (concat text "\n\n" (scooter--region-quote))))

;;;###autoload
(defun scooter-send-region (server id text)
  "Send the region with TEXT to SERVER's conversation ID."
  (interactive (let ((server (scooter-current-server current-prefix-arg)))
                 (list server (scooter-read-conversation server) (read-string "About the region: "))))
  (scooter-request server "POST" (format "/conversations/%s/messages" id)
                   `((text . ,(concat text "\n\n" (scooter--region-quote)))))
  (message "Sent to %s" id))

(declare-function projectile-project-root "projectile")
(defvar projectile-mode)

(defun scooter-project-root ()
  "Return the current project's root, preferring Projectile."
  (or (and (bound-and-true-p projectile-mode) (projectile-project-root))
      (when-let* ((pr (project-current))) (project-root pr))
      (user-error "Not in a project")))

(defun scooter-project-context (root)
  "Describe ROOT's git remote and branch for the agent."
  (let* ((default-directory root)
         (remote (string-trim (shell-command-to-string "git remote get-url origin 2>/dev/null")))
         (branch (string-trim (shell-command-to-string "git rev-parse --abbrev-ref HEAD 2>/dev/null"))))
    (when (string-empty-p remote) (user-error "%s has no origin remote" root))
    (format "I'm working in %s on branch %s. Clone it into /workspace and check out that branch."
            remote branch)))

;;;###autoload
(defun scooter-new-from-project (server text)
  "Start a conversation on SERVER about the current project, with TEXT."
  (interactive (list (scooter-current-server current-prefix-arg) (read-string "Task: ")))
  (scooter-new server (concat (scooter-project-context (scooter-project-root)) "\n\n" text)))

;;;###autoload (autoload 'scooter-dispatch "scooter" nil t)
(transient-define-prefix scooter-dispatch ()
  "Scooter commands."
  [["Conversations"
    ("l" "List" scooter-list)
    ("o" "Open" scooter-open)
    ("c" "New" scooter-new)
    ("f" "Find file" scooter-find-file)]
   ["This conversation"
    ("b" "Sandbox" scooter-sandbox)
    ("w" "Web services" scooter-web-services)
    ("k" "Links" scooter-links)]
   ["From here"
    ("r" "New from region" scooter-new-from-region)
    ("R" "Send region" scooter-send-region)
    ("p" "New from project" scooter-new-from-project)]
   ["Servers"
    ("S" "Default server" scooter-select-server)
    ("L" "Log in" scooter-auth-login)
    ("C" "Set cookie" scooter-auth-set-cookie)
    ("F" "Forget credentials" scooter-auth-forget)]])

;;;###autoload
(defvar-keymap scooter-command-map
  :doc "Scooter commands; bind it to a prefix."
  "s" #'scooter-dispatch
  "l" #'scooter-list
  "o" #'scooter-open
  "c" #'scooter-new
  "f" #'scooter-find-file
  "r" #'scooter-new-from-region
  "R" #'scooter-send-region
  "p" #'scooter-new-from-project
  "b" #'scooter-sandbox
  "w" #'scooter-web-services
  "k" #'scooter-links)

(provide 'scooter)
;;; scooter.el ends here
