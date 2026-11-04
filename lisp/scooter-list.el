;;; scooter-list.el --- Conversation list for Scooter  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;;; Commentary:

;; `scooter-list' shows each server's conversations as a table of
;; `magit-section's, starred ones first, under a row of column names per
;; server.  An icon gives each conversation's status, as in the web UI,
;; and each row folds open to the conversation's details.  The list
;; refreshes when /conversations/events reports a change.

;;; Code:

(require 'cl-lib)
(require 'magit-section)
(require 'scooter-server)
(require 'scooter-http)
(require 'transient)

(declare-function scooter-open "scooter" (server id))
(declare-function scooter-new "scooter" (server text &optional model))
(declare-function scooter-dispatch "scooter")
(declare-function scooter-sandbox "scooter-conversation")
(declare-function scooter-web-services "scooter-conversation")
(declare-function scooter-links "scooter-conversation")
(declare-function evil-set-initial-state "evil-core" (mode state))

(defface scooter-list-awaiting '((t :inherit (warning bold)))
  "Face for the icon of a conversation waiting on you."
  :group 'scooter)

(defface scooter-list-running '((t :inherit success))
  "Face for the icon of a conversation whose agent is working."
  :group 'scooter)

(defface scooter-list-idle '((t :inherit shadow))
  "Face for the icon of an idle conversation with a running sandbox."
  :group 'scooter)

(defface scooter-list-failed '((t :inherit error))
  "Face for the icon of a conversation whose sandbox failed."
  :group 'scooter)

(defface scooter-list-star '((t :inherit warning))
  "Face for the star on starred conversations."
  :group 'scooter)

(defface scooter-list-model '((t :inherit font-lock-type-face))
  "Face for the model column."
  :group 'scooter)

(defface scooter-list-dim '((t :inherit shadow))
  "Face for secondary columns."
  :group 'scooter)

(defface scooter-list-server '((t :inherit (magit-section-heading bold) :height 1.15))
  "Face for server names."
  :group 'scooter)

(defface scooter-list-header '((t :inherit (bold shadow) :underline t))
  "Face for the row of column names under each server."
  :group 'scooter)

(defface scooter-list-link '((t :inherit link :underline nil))
  "Face for the Links column."
  :group 'scooter)

(defconst scooter-list--statuses
  '((awaiting "!" scooter-list-awaiting "Waiting on you"
              "The agent asked a question or needs an approval.")
    (running "●" scooter-list-running "Agent working" "A turn is in progress.")
    (idle "●" scooter-list-idle "Sandbox running"
          "The agent is idle but its sandbox is still up.")
    (suspended "○" scooter-list-idle "Sandbox suspended" "Its sandbox is suspended.")
    (failed "●" scooter-list-failed "Sandbox failed" "The sandbox could not run."))
  "Each status's icon, icon face, label and hint, as in the web UI's sidebar.")

(defvar-local scooter-list--servers nil
  "Servers this buffer shows.")

(defvar-local scooter-list--streams nil
  "Live event stream processes, one per server.")

(defvar-local scooter-list--refresh-timer nil
  "Pending debounced refresh.")

(defun scooter-list-status (conv)
  "Return the status symbol for CONV, an alist, ranked as the web UI does."
  (let ((status (alist-get 'status conv)))
    (cond
     ((equal status "failed") 'failed)
     ((alist-get 'awaitingInputAt conv) 'awaiting)
     ((equal status "suspended") 'suspended)
     ((alist-get 'runStartedAt conv) 'running)
     (t 'idle))))

(defun scooter-slug (title &optional id)
  "Return the slug for TITLE, or ID's first 8 characters when untitled."
  (let ((slug (string-trim (replace-regexp-in-string "[^a-z0-9]+" "-" (downcase (or title ""))) "-+" "-+")))
    (if (string-empty-p slug)
        (substring (or id "") 0 (min 8 (length (or id ""))))
      (string-trim-right (substring slug 0 (min 40 (length slug))) "-+"))))

(defun scooter-list--age (ms)
  "Format epoch MS as a short age."
  (if (not (numberp ms)) ""
    (let ((s (- (float-time) (/ ms 1000.0))))
      (cond ((< s 60) "<1m")
            ((< s 3600) (format "%dm" (/ s 60)))
            ((< s 86400) (format "%dh" (/ s 3600)))
            (t (format "%dd" (/ s 86400)))))))

(defclass scooter-server-section (magit-section) ())
(defclass scooter-group-section (magit-section) ())
(defclass scooter-conversation-section (magit-section) ())

(cl-defmethod magit-section-ident-value ((section scooter-conversation-section))
  "Identify SECTION by server and id, so expanded rows survive refreshes."
  (let ((value (oref section value)))
    (cons (car value) (alist-get 'id (cdr value)))))

(defvar-keymap scooter-list-mode-map
  :doc "Keymap for `scooter-list-mode'."
  :parent magit-section-mode-map
  "RET" #'scooter-list-open
  "c" #'scooter-list-new
  "s" #'scooter-list-star
  "t" #'scooter-list-retitle
  "k" #'scooter-list-cancel
  "z" #'scooter-list-suspend
  "f" #'scooter-list-files
  "w" #'scooter-list-copy-url
  "b" #'scooter-list-browse
  "m" #'scooter-sandbox
  "v" #'scooter-web-services
  "l" #'scooter-links
  "g" #'scooter-list-refresh
  "?" #'scooter-list-dispatch)

(define-derived-mode scooter-list-mode magit-section-mode "Scooter"
  "Scooter conversations by server, starred ones first."
  (setq-local revert-buffer-function (lambda (&rest _) (scooter-list-refresh)))
  (add-hook 'window-size-change-functions #'scooter-list--refit nil t)
  (add-hook 'kill-buffer-hook #'scooter-list--stop-streams nil t))

;; Evil's normal state would shadow every key here, `?' included.
(with-eval-after-load 'evil
  (evil-set-initial-state 'scooter-list-mode 'emacs))

(transient-define-prefix scooter-list-dispatch ()
  "Commands for the conversation list."
  [["Conversation at point"
    ("RET" "Open" scooter-list-open)
    ("s" "Star" scooter-list-star)
    ("t" "Retitle" scooter-list-retitle)
    ("k" "Stop run" scooter-list-cancel)
    ("z" "Suspend" scooter-list-suspend)
    ("m" "Sandbox…" scooter-sandbox)
    ("v" "Web services" scooter-web-services)]
   ["Links"
    ("f" "Files" scooter-list-files)
    ("b" "Browse" scooter-list-browse)
    ("w" "Copy URL" scooter-list-copy-url)
    ("l" "Linked resources" scooter-links)]
   ["List"
    ("c" "New" scooter-list-new)
    ("g" "Refresh" scooter-list-refresh)
    ("S" "More Scooter commands" scooter-dispatch)]])

;;;###autoload
(defun scooter-list (&optional server)
  "List conversations on every server, or only SERVER with a prefix argument."
  (interactive (list (and current-prefix-arg (scooter-read-server))))
  (let ((buf (get-buffer-create (if server (format "*scooter: %s*" server) "*scooter*"))))
    (with-current-buffer buf
      (unless (derived-mode-p 'scooter-list-mode) (scooter-list-mode))
      ;; Self-access cannot list conversations.
      (setq scooter-list--servers (if server (list server)
                                    (seq-remove (lambda (s) (scooter-server-get s :self))
                                                (scooter-server-names))))
      (scooter-list-refresh)
      (scooter-list--start-streams))
    (pop-to-buffer-same-window buf)))

(defun scooter-list--fetch (server)
  "Return SERVER's conversations, or an error string."
  (condition-case err
      (cl-sort (seq-remove (lambda (c) (alist-get 'parentId c))
                           (scooter-request server "GET" "/conversations"))
               #'> :key (lambda (c) (or (alist-get 'lastActivityAt c) 0)))
    (error (error-message-string err))))

(defconst scooter-list--columns
  '(("" 1 scooter-list--col-status)
    ("Title" title scooter-list--col-title)
    ("Model" 18 scooter-list--col-model)
    ("Links" 14 scooter-list--col-links)
    ("Owner" 10 scooter-list--col-owner)
    ("Active" 6 scooter-list--col-active))
  "Table columns as (LABEL WIDTH CELL-FUNCTION).
WIDTH `title' takes whatever the window leaves.  CELL-FUNCTION takes a
conversation alist and returns a string.")

(defcustom scooter-list-min-title-width 24
  "Narrowest the Title column gets, however small the window."
  :type 'natnum
  :group 'scooter)

(defvar-local scooter-list--title-width 40
  "Title column width for the current render.")

(defvar-local scooter-list--data nil
  "The last rendered (SERVER . CONVERSATIONS-OR-ERROR) list.")

(defun scooter-list--title-width ()
  "Return the Title column width that fills this buffer's window."
  (let ((fixed (cl-loop for (_ w) in scooter-list--columns
                        sum (if (numberp w) (1+ w) 1)))
        (win (or (get-buffer-window) (selected-window))))
    (max scooter-list-min-title-width (- (window-max-chars-per-line win) fixed))))

(defun scooter-list--pad-to (line col)
  "Return LINE extended with space to column COL.
The space is a display stop, so columns align in proportional fonts too."
  (concat line (propertize (make-string (max 1 (- col (string-width line))) ?\s)
                           'display `(space :align-to ,col))))

(defun scooter-list--cells (cells)
  "Join CELLS, one string per column, each padded to its column's width."
  (let ((line "") (col 0))
    (cl-loop for (_ w) in scooter-list--columns
             for cell in cells
             for width = (if (numberp w) w scooter-list--title-width)
             do (setq col (+ col width 1)
                      line (scooter-list--pad-to
                            (concat line (truncate-string-to-width cell width nil nil "…"))
                            col)))
    line))

(defun scooter-list--row (conv)
  "Return CONV's table row, without a newline."
  (string-trim-right
   (scooter-list--cells (mapcar (lambda (col) (funcall (nth 2 col) conv))
                                scooter-list--columns))))

(defun scooter-list--header ()
  "Return the row naming each column."
  (propertize (string-trim-right (scooter-list--cells (mapcar #'car scooter-list--columns)))
              'font-lock-face 'scooter-list-header))

(defun scooter-list--col-status (conv)
  "Status icon for CONV, its meaning in the tooltip."
  (pcase-let ((`(,_ ,icon ,face) (assq (scooter-list-status conv) scooter-list--statuses)))
    (propertize icon 'font-lock-face face 'help-echo (scooter-list--state conv t))))

(defun scooter-list--col-title (conv)
  "Title cell for CONV, falling back to its short id."
  (let ((title (alist-get 'title conv)))
    (if (and title (not (string-empty-p title))) title
      (propertize (scooter-slug nil (alist-get 'id conv)) 'font-lock-face 'scooter-list-dim))))

(defun scooter-list--state (conv &optional hint)
  "Describe CONV's status and how long it has held, with its HINT if non-nil."
  (let* ((status (scooter-list-status conv))
         (spec (assq status scooter-list--statuses)))
    (cl-flet ((since (key fmt)
                (let ((ms (alist-get key conv)))
                  (if (numberp ms) (format fmt (scooter-list--age ms)) ""))))
      (concat (nth 3 spec)
              (pcase status
                ('awaiting (since 'awaitingInputAt " for %s"))
                ('running (since 'runStartedAt " for %s"))
                ('idle (since 'lastReplyAt ", last reply %s ago")))
              (and hint (concat "\n" (nth 4 spec)))))))

(defun scooter-list--col-model (conv)
  "Model cell for CONV."
  (if-let* ((model (alist-get 'model conv)))
      (propertize model 'font-lock-face 'scooter-list-model)
    (propertize "(default)" 'font-lock-face 'scooter-list-dim)))

(defun scooter-list--link-label (link)
  "Short label for LINK: a PR or issue number, else its source."
  (let ((url (or (alist-get 'url link) "")))
    (if (string-match "/\\(?:pull\\|issues\\|merge_requests\\)/\\([0-9]+\\)" url)
        (format "#%s" (match-string 1 url))
      (alist-get 'source link))))

(defun scooter-list--links (conv)
  "Return CONV's linked resources, from links or else bare sources."
  (or (append (alist-get 'links conv) nil)
      (mapcar (lambda (s) `((source . ,s))) (alist-get 'sources conv))))

(defun scooter-list--col-links (conv)
  "Links cell for CONV."
  (propertize (string-join (delete-dups (delq nil (mapcar #'scooter-list--link-label
                                                          (scooter-list--links conv))))
                           " ")
              'font-lock-face 'scooter-list-link))

(defun scooter-list--col-owner (conv)
  "Owner cell for CONV, its user part when an email."
  (propertize (car (split-string (or (alist-get 'owner conv) "") "@"))
              'font-lock-face 'scooter-list-dim))

(defun scooter-list--col-active (conv)
  "Last-activity cell for CONV."
  (propertize (scooter-list--age (alist-get 'lastActivityAt conv)) 'font-lock-face 'scooter-list-dim))

(defun scooter-list--date (ms)
  "Format epoch MS as a local date and time."
  (format-time-string "%Y-%m-%d %H:%M" (/ ms 1000.0)))

(defun scooter-list--insert-detail (label &rest fields)
  "Insert a details line: LABEL, then FIELDS at successive tab stops.
Insert nothing when the last field is empty."
  (let ((value (car (last fields))))
    (unless (string-empty-p (or value ""))
      (let ((line (scooter-list--pad-to "" 5)))
        (cl-loop for text in (cons (propertize label 'font-lock-face 'scooter-list-dim) (butlast fields))
                 for col in '(16 26 42)
                 do (setq line (scooter-list--pad-to (concat line text) col)))
        (insert line value "\n")))))

(defun scooter-list--insert-details (server conv)
  "Insert CONV's details, shown when its row on SERVER is expanded."
  (let ((created (alist-get 'createdAt conv))
        (label "links"))
    (scooter-list--insert-detail "id" (propertize (alist-get 'id conv) 'font-lock-face 'scooter-list-model))
    (scooter-list--insert-detail "title" (alist-get 'title conv))
    (when (numberp created)
      (scooter-list--insert-detail
       "created" (format "%s (%s ago)" (scooter-list--date created) (scooter-list--age created))))
    (scooter-list--insert-detail
     "state" (string-join (delq nil (list (scooter-list--state conv) (alist-get 'visibility conv)))
                          " · "))
    (scooter-list--insert-detail "owner" (alist-get 'owner conv))
    (dolist (link (scooter-list--links conv))
      (let ((short (scooter-list--link-label link))
            (source (alist-get 'source link)))
        (scooter-list--insert-detail
         label (or source "") (or (alist-get 'resourceType link) "")
         (string-join (delq nil (list (and (not (equal short source))
                                           (propertize short 'font-lock-face 'scooter-list-link))
                                      (or (alist-get 'title link) (alist-get 'url link))))
                      " ")))
      (setq label ""))
    (scooter-list--insert-detail
     "url" (propertize (scooter-server-url server (format "/chat/%s" (alist-get 'id conv)))
                       'font-lock-face 'link))))

(defun scooter-list--insert-conversation (server conv)
  "Insert one row for CONV on SERVER, its details folded beneath."
  (magit-insert-section (scooter-conversation-section (cons server conv) t)
    (magit-insert-heading (scooter-list--row conv))
    (magit-insert-section-body
      (scooter-list--insert-details server conv))))

(defun scooter-list--render (data)
  "Render DATA, a list of (SERVER . CONVERSATIONS-OR-ERROR)."
  (let ((inhibit-read-only t)
        (pos (point)))
    (setq scooter-list--data data
          scooter-list--title-width (scooter-list--title-width))
    (erase-buffer)
    (magit-insert-section (root)
      (pcase-dolist (`(,server . ,convs) data)
        (magit-insert-section (scooter-server-section server)
          (magit-insert-heading
            (propertize (format "%s" server) 'font-lock-face 'scooter-list-server)
            (propertize (format "  %s" (scooter-server-url server)) 'font-lock-face 'scooter-list-dim)
            (propertize (if (stringp convs) "  unreachable"
                          (format "  %d conversation%s" (length convs) (if (= (length convs) 1) "" "s")))
                        'font-lock-face 'scooter-list-dim))
          (if (stringp convs)
              (insert "  " (propertize convs 'font-lock-face 'error) "\n")
            (insert (scooter-list--header) "\n")
            (pcase-dolist (`(,group ,icon ,label ,members)
                           (let ((starred (seq-group-by (lambda (c) (eq (alist-get 'starred c) t)) convs)))
                             `((starred "★ " "Starred" ,(alist-get t starred))
                               (recent "" "Recent" ,(alist-get nil starred)))))
              (when members
                (magit-insert-section (scooter-group-section group)
                  (magit-insert-heading
                    (propertize icon 'font-lock-face 'scooter-list-star)
                    (propertize (format "%s (%d)" label (length members))
                                'font-lock-face 'magit-section-heading))
                  (dolist (c members) (scooter-list--insert-conversation server c))))))
          (insert "\n"))))
    (goto-char (min pos (point-max)))))

(defun scooter-list--refit (win)
  "Redraw WIN's list when its width changes the Title column."
  (with-current-buffer (window-buffer win)
    (when (and scooter-list--data
               (/= scooter-list--title-width (scooter-list--title-width)))
      (save-excursion (scooter-list--render scooter-list--data)))))

(defun scooter-list-refresh ()
  "Refetch and redraw the list."
  (interactive)
  (scooter-list--render
   (mapcar (lambda (s) (cons s (scooter-list--fetch s))) scooter-list--servers)))

(defun scooter-list--schedule-refresh (buf)
  "Refresh BUF soon, coalescing bursts of change events."
  (when (buffer-live-p buf)
    (with-current-buffer buf
      (unless (timerp scooter-list--refresh-timer)
        (setq scooter-list--refresh-timer
              (run-at-time 1 nil
                           (lambda ()
                             (when (buffer-live-p buf)
                               (with-current-buffer buf
                                 (setq scooter-list--refresh-timer nil)
                                 (ignore-errors (scooter-list-refresh)))))))))))

(defun scooter-list--start-streams ()
  "Follow each shown server's change stream."
  (scooter-list--stop-streams)
  (let ((buf (current-buffer)))
    (dolist (server scooter-list--servers)
      (scooter-list--follow buf server))))

(defun scooter-list--follow (buf server)
  "Follow SERVER's change stream for BUF, reconnecting while BUF lives."
  (when (buffer-live-p buf)
    (let ((proc (ignore-errors
                  (scooter-http-stream server "/conversations/events"
                                       (lambda (frame)
                                         (unless (equal (alist-get 'kind frame) "snapshot")
                                           (scooter-list--schedule-refresh buf)))))))
      (when proc
        (set-process-sentinel
         proc (lambda (_p _e)
                (when (buffer-live-p buf)
                  (run-at-time 10 nil #'scooter-list--follow buf server))))
        (with-current-buffer buf (push proc scooter-list--streams))))))

(defun scooter-list--stop-streams ()
  "Stop this buffer's change streams."
  (dolist (p scooter-list--streams)
    (set-process-sentinel p #'ignore)
    (delete-process p))
  (setq scooter-list--streams nil))

(defun scooter-list--at-point ()
  "Return (SERVER . CONVERSATION) at point, or signal."
  (let ((section (magit-current-section)))
    (or (and (cl-typep section 'scooter-conversation-section) (oref section value))
        (user-error "No conversation here"))))

(defun scooter-list--server-at-point ()
  "Return the server whose section contains point."
  (let ((s (magit-current-section)))
    (while (and s (not (cl-typep s 'scooter-server-section)))
      (setq s (oref s parent)))
    (if s (oref s value) (scooter-current-server))))

(defun scooter-list--id (entry) (alist-get 'id (cdr entry)))

(defun scooter-list-open ()
  "Open the conversation at point in agent-shell."
  (interactive)
  (let ((e (scooter-list--at-point)))
    (scooter-open (car e) (scooter-list--id e))))

(defun scooter-list-new (text)
  "Start a conversation on the server at point with TEXT."
  (interactive (list (read-string "New conversation: ")))
  (scooter-new (scooter-list--server-at-point) text))

(defun scooter-list-star ()
  "Toggle the star on the conversation at point."
  (interactive)
  (let ((e (scooter-list--at-point)))
    (scooter-request (car e) "PATCH" (format "/conversations/%s/starred" (scooter-list--id e))
                     `((starred . ,(if (eq (alist-get 'starred (cdr e)) t) :json-false t))))
    (scooter-list-refresh)))

(defun scooter-list-retitle (title)
  "Set the conversation at point's TITLE."
  (interactive (list (read-string "Title: " (alist-get 'title (cdr (scooter-list--at-point))))))
  (let ((e (scooter-list--at-point)))
    (scooter-request (car e) "PATCH" (format "/conversations/%s/title" (scooter-list--id e))
                     `((title . ,title)))
    (scooter-list-refresh)))

(defun scooter-list-cancel ()
  "Stop the run of the conversation at point."
  (interactive)
  (let ((e (scooter-list--at-point)))
    (when (yes-or-no-p (format "Stop the run in %s? " (scooter-slug (alist-get 'title (cdr e)) (scooter-list--id e))))
      (scooter-request (car e) "POST" (format "/conversations/%s/cancel" (scooter-list--id e))))))

(defun scooter-list-suspend ()
  "Suspend the sandbox of the conversation at point."
  (interactive)
  (let ((e (scooter-list--at-point)))
    (scooter-request (car e) "POST" (format "/conversations/%s/suspend" (scooter-list--id e)))
    (scooter-list-refresh)))

(defun scooter-list--url (entry &optional files)
  "Return ENTRY's browser URL, or its files viewer when FILES."
  (scooter-server-url (car entry)
                      (if files (format "/c/%s/files/" (scooter-list--id entry))
                        (format "/chat/%s" (scooter-list--id entry)))))

(defun scooter-list-files ()
  "Open the files viewer of the conversation at point in a browser."
  (interactive)
  (browse-url (scooter-list--url (scooter-list--at-point) t)))

(defun scooter-list-browse ()
  "Open the conversation at point in a browser."
  (interactive)
  (browse-url (scooter-list--url (scooter-list--at-point))))

(defun scooter-list-copy-url ()
  "Copy the browser URL of the conversation at point."
  (interactive)
  (let ((url (scooter-list--url (scooter-list--at-point))))
    (kill-new url)
    (message "Copied %s" url)))

(provide 'scooter-list)
;;; scooter-list.el ends here
