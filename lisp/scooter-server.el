;;; scooter-server.el --- Scooter server registry  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;;; Commentary:

;; `scooter-servers' names each Scooter deployment.  Every Scooter buffer is
;; bound to one server through `scooter--server'.

;;; Code:

(require 'cl-lib)
(require 'url-parse)

(defgroup scooter nil
  "Use Scooter from Emacs."
  :group 'tools
  :prefix "scooter-")

(defcustom scooter-servers nil
  "Scooter servers, as a list of (NAME . PLIST).

NAME is a symbol.  PLIST keys:
  :url    the server's base URL (required)
  :auth   nil, `cookie', (cookie :browser firefox) to take the cookie
          from Firefox, or (oidc :issuer URL :client-id ID [:scopes LIST])
  :model  default model for new conversations
  :self   non-nil inside a Scooter sandbox: chat with its own
          conversation via the router's self-access (no :url)

Example:

  (setq scooter-servers
        \\='((home :url \"https://scooter.example.org\" :auth cookie)
          (work :url \"https://scooter.corp.example\"
                :auth (oidc :issuer \"https://oauth.id.jumpcloud.com/\"
                            :client-id \"scooter-emacs\"))))"
  :type '(alist :key-type symbol :value-type plist))

(defvar scooter-default-server nil
  "The server commands use when none is bound to the buffer.")

(defvar-local scooter--server nil
  "The server this buffer belongs to.")

(defun scooter-server-names ()
  "Return the configured server names."
  (mapcar #'car scooter-servers))

(defun scooter-server--name (server)
  "Return SERVER (a symbol or string) as a symbol."
  (if (stringp server) (intern server) server))

(defun scooter-server-plist (server)
  "Return SERVER's settings, or signal an error."
  (or (alist-get (scooter-server--name server) scooter-servers)
      (user-error "Unknown Scooter server: %s" server)))

(defun scooter-server-get (server key)
  "Return SERVER's KEY setting."
  (plist-get (scooter-server-plist server) key))

(defun scooter-server-url (server &optional path)
  "Return SERVER's base URL, with PATH appended."
  (concat (string-trim-right (scooter-server-get server :url) "/") path))

(defun scooter-server-host (server)
  "Return the host name of SERVER's URL."
  (url-host (url-generic-parse-url (scooter-server-url server))))

(defun scooter-read-server (&optional prompt)
  "Read a server name with PROMPT."
  (let ((names (scooter-server-names)))
    (unless names
      (user-error "No Scooter servers configured; set `scooter-servers'"))
    (intern (completing-read (or prompt "Scooter server: ") names nil t nil nil
                             (and scooter-default-server
                                  (symbol-name scooter-default-server))))))

(defun scooter-current-server (&optional ask)
  "Return the server for this buffer, reading one if ASK or ambiguous."
  (cond
   (ask (scooter-read-server))
   (scooter--server)
   (scooter-default-server)
   ((= (length scooter-servers) 1) (caar scooter-servers))
   (t (scooter-read-server))))

(defun scooter-select-server (server)
  "Make SERVER the default."
  (interactive (list (scooter-read-server "Default Scooter server: ")))
  (setq scooter-default-server server)
  (message "Scooter: default server is %s" server))

(provide 'scooter-server)
;;; scooter-server.el ends here
