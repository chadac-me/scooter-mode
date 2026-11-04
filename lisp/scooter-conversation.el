;;; scooter-conversation.el --- Act on one Scooter conversation  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;;; Commentary:

;; Commands for one conversation: its sandbox (size, suspend, restart,
;; compact, title, star, visibility), its web services and its links.
;; Each acts on the conversation of the Scooter shell you are in, the row
;; at point in `scooter-list', or one you pick.

;;; Code:

(require 'cl-lib)
(require 'map)
(require 'browse-url)
(require 'url-util)
(require 'url-expand)
(require 'scooter-server)
(require 'scooter-http)
(require 'scooter-list)
(require 'scooter-agent-shell)
(require 'scooter)

(defun scooter-conversation-target (&optional ask)
  "Return (SERVER ID) for the conversation these commands act on.
That is this Scooter shell's, the row at point in `scooter-list', or,
with ASK or anywhere else, one read from a server."
  (or (and (not ask) (scooter-agent-shell-conversation))
      (and (not ask) (derived-mode-p 'scooter-list-mode)
           (let ((e (scooter-list--at-point)))
             (list (car e) (scooter-list--id e))))
      (let ((server (scooter-current-server ask)))
        (list server (scooter-read-conversation server)))))

(defun scooter-conversation--path (id &optional tail)
  "Return conversation ID's API path, followed by TAIL."
  (concat "/conversations/" (url-hexify-string id) tail))

(defun scooter-conversation--get (server id &optional tail)
  "GET conversation ID's TAIL on SERVER, or nil when the server has none."
  (condition-case nil
      (scooter-request server "GET" (scooter-conversation--path id tail))
    (scooter-http-error nil)))

;;;; Sandbox

(defun scooter-conversation--describe (conv resources ready)
  "Summarize CONV's sandbox from its RESOURCES and READY replies."
  (let-alist conv
    (let ((cpu (map-nested-elt resources '(resources limits cpu)))
          (memory (map-nested-elt resources '(resources limits memory))))
      (format "%s: %s%s, %s, %s%s"
              (or .title .id)
              (or .status "unknown")
              (if (eq (alist-get 'ready ready) t) " (ready)" "")
              (or .visibility "shared")
              (if cpu (format "%s CPU, %s" cpu memory) "default size")
              (if (eq (alist-get 'pendingRestart resources) t) ", restart pending" "")))))

(defun scooter-conversation--actions (conv sizes)
  "Return the sandbox actions that apply to CONV, given the SIZES presets."
  (let-alist conv
    (delq nil (list (and (alist-get 'sizes sizes) "resize")
                    (if (equal .status "suspended") "resume" "suspend")
                    "restart" "compact" "rename"
                    (if (eq .starred t) "unstar" "star")
                    (if (equal .visibility "private") "make shared" "make private")
                    "browse" "files"))))

(defun scooter-conversation--busy-p (conv)
  "Non-nil when CONV has a run going."
  (alist-get 'runStartedAt conv))

(defun scooter-conversation--run (server id action conv sizes)
  "Run sandbox ACTION on SERVER's conversation ID, described by CONV and SIZES."
  (let ((call (lambda (method tail &optional body)
                (scooter-request server method (scooter-conversation--path id tail) body))))
    (when (and (member action '("suspend" "restart" "compact")) (scooter-conversation--busy-p conv))
      (user-error "A run is going; stop it first (`scooter-list-cancel' or C-c C-c in its shell)"))
    (pcase action
      ("resize"
       (let ((size (completing-read "Sandbox size: "
                                    (mapcar (lambda (s) (format "%s" (car s))) (alist-get 'sizes sizes))
                                    nil t nil nil (alist-get 'default sizes))))
         (funcall call "PATCH" "/size" `((size . ,size)))
         (message "Scooter: size %s applies on the next restart" size)))
      ((or "suspend" "resume")
       (funcall call "POST" (concat "/" action) '())
       (message "Scooter: %s requested" action))
      ("restart"
       (when (yes-or-no-p "Restart the sandbox? Processes running in it are lost. ")
         (funcall call "POST" "/restart" '())
         (message "Scooter: restarting")))
      ("compact"
       (when (yes-or-no-p "Compact the conversation into a summary? ")
         (let-alist (funcall call "POST" "/compact" '())
           (message "Scooter: %s" (if (eq .compacted t) (format "compacted %s turns" .summarizedTurns)
                                    (or .reason "nothing to compact"))))))
      ("rename"
       (funcall call "PATCH" "/title" `((title . ,(read-string "Title: " (alist-get 'title conv))))))
      ("star" (funcall call "PATCH" "/starred" '((starred . t))))
      ("unstar" (funcall call "PATCH" "/starred" '((starred . :json-false))))
      ("make shared"
       (when (yes-or-no-p "Make this private conversation shared? What it fetched while private becomes visible. ")
         (funcall call "PATCH" "/visibility" '((visibility . "shared") (confirm . t)))))
      ("make private" (funcall call "PATCH" "/visibility" '((visibility . "private"))))
      ("browse" (browse-url (scooter-server-url server (format "/chat/%s" id))))
      ("files" (browse-url (scooter-server-url server (format "/c/%s/files/" id)))))))

;;;###autoload
(defun scooter-sandbox (server id)
  "Show SERVER's conversation ID's sandbox and pick an action for it.
In a Scooter shell or `scooter-list', act on that conversation; with a
prefix argument, pick one."
  (interactive (scooter-conversation-target current-prefix-arg))
  (let* ((conv (scooter-request server "GET" (scooter-conversation--path id)))
         (resources (scooter-conversation--get server id "/resources"))
         (ready (scooter-conversation--get server id "/ready"))
         (sizes (condition-case nil (scooter-request server "GET" "/sandbox-sizes")
                  (scooter-http-error nil))))
    (scooter-conversation--run
     server id
     (completing-read (concat (scooter-conversation--describe conv resources ready) " > ")
                      (scooter-conversation--actions conv sizes) nil t)
     conv sizes)))

;;;; Web services

(defun scooter-conversation--services (server id)
  "Return SERVER's conversation ID's web services, with absolute URLs."
  (mapcar (lambda (s)
            (let ((url (alist-get 'url s)))
              (cons `(url . ,(if url (url-expand-file-name url (scooter-server-url server "/")) ""))
                    s)))
          (alist-get 'services
                     (scooter-request server "GET" (scooter-conversation--path id "/web-services?refresh=1")))))

;;;###autoload
(defun scooter-web-services (server id)
  "Open, copy, start or stop one of conversation ID's web services on SERVER.
The conversation is chosen as for `scooter-sandbox'."
  (interactive (scooter-conversation-target current-prefix-arg))
  (let* ((services (or (scooter-conversation--services server id)
                       (user-error "This conversation declares no web services")))
         (table (mapcar (lambda (s)
                          (cons (format "%-24s %-8s %s"
                                        (or (alist-get 'displayName s) (alist-get 'name s))
                                        (if (eq (alist-get 'running s) t) "running" "stopped")
                                        (alist-get 'url s))
                                s))
                        services))
         (service (cdr (assoc (completing-read "Web service: " table nil t) table)))
         (name (alist-get 'name service))
         (url (alist-get 'url service))
         (toggle (if (eq (alist-get 'running service) t) "stop" "start")))
    (pcase (car (read-multiple-choice (format "%s (%s)" name url)
                                      `((?b "browse" "open it in the browser")
                                        (?c "copy" "copy its URL")
                                        (?s ,toggle ,(format "%s the service" toggle)))))
      (?b (browse-url url))
      (?c (kill-new url) (message "Copied %s" url))
      (?s (scooter-request server "POST"
                           (scooter-conversation--path id (format "/web-services/%s/%s" (url-hexify-string name) toggle)))
          (message "Scooter: %s %s requested" toggle name)))))

;;;; Links

(defun scooter-conversation-link-label (link)
  "Return LINK's label, as `scooter-list' shows it."
  (let-alist link
    (format "%s: %s" .source (or .title (string-replace "_" " " (or .resourceType "link"))))))

;;;###autoload
(defun scooter-links (server id &optional copy)
  "Open a resource linked to conversation ID on SERVER; with COPY, copy its URL.
In a Scooter shell or `scooter-list', act on that conversation; a
prefix argument copies instead of opening."
  (interactive (append (scooter-conversation-target) (list current-prefix-arg)))
  (let* ((links (seq-filter (lambda (l) (alist-get 'url l))
                            (alist-get 'links (scooter-request server "GET" (scooter-conversation--path id "/links")))))
         (table (or (mapcar (lambda (l) (cons (scooter-conversation-link-label l) (alist-get 'url l))) links)
                    (user-error "No links on this conversation")))
         (url (cdr (assoc (completing-read (if copy "Copy link: " "Open link: ") table nil t) table))))
    (if copy
        (progn (kill-new url) (message "Copied %s" url))
      (browse-url url))))

(provide 'scooter-conversation)
;;; scooter-conversation.el ends here
