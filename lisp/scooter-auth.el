;;; scooter-auth.el --- Auth for Scooter behind oauth2-proxy  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;;; Commentary:

;; Backends, chosen per server by its :auth setting:
;;
;; - `cookie': the `_oauth2_proxy' session cookie, pasted or read from
;;   auth-source, and saved there.  Works with any oauth2-proxy, GitHub-backed
;;   ones included.
;; - (cookie :browser firefox): the same cookie, taken from the default
;;   Firefox profile; when it has no live session, the sign-in page opens
;;   there and Emacs waits for the cookie to appear.
;; - (oidc :issuer URL :client-id ID): an ID token from Authorization Code +
;;   PKCE with a loopback redirect, sent as a bearer token.  oauth2-proxy
;;   must run with --skip-jwt-bearer-tokens.  The refresh token is kept in
;;   auth-source.

;;; Code:

(require 'cl-lib)
(require 'seq)
(require 'auth-source)
(require 'browse-url)
(require 'url-util)
(require 'scooter-server)
(require 'scooter-http)

(defcustom scooter-auth-login-timeout 300
  "Seconds to wait for a browser login to finish."
  :type 'integer
  :group 'scooter)

(declare-function secrets-search-items "secrets")
(declare-function secrets-delete-item "secrets")

(defvar scooter-auth--cache (make-hash-table :test #'eq)
  "Server name to its auth state plist.")

(defun scooter-auth--backend (server)
  "Return SERVER's backend symbol and its settings, as (TYPE . PLIST)."
  (let ((auth (scooter-server-get server :auth)))
    (if (consp auth) auth (list auth))))

(defun scooter-auth-headers (server &optional retry)
  "Return the auth headers for SERVER as an alist.
RETRY non-nil means the last headers hit the auth wall."
  (when retry (scooter-auth-forget server))
  (pcase (scooter-auth--backend server)
    (`(nil) nil)
    (`(cookie . ,_) `(("Cookie" . ,(concat "_oauth2_proxy=" (scooter-auth--cookie server)))))
    (`(oidc . ,opts) `(("Authorization" . ,(concat "Bearer " (scooter-auth--id-token server opts)))))
    (other (user-error "Scooter[%s]: unknown :auth %S" server other))))

;;;###autoload
(defun scooter-auth-headers-json (server &optional retry)
  "Return SERVER's auth headers as a JSON string, for `scooter acp'.
RETRY is as in `scooter-auth-headers'.  While a sign-in for SERVER is
already waiting on the user, return \"null\": the bridge asks again
shortly instead of opening a second prompt or failing its request."
  (let ((json-encoding-pretty-print nil))
    (condition-case nil
        (let ((headers (scooter-auth-headers
                        server (and retry (not (scooter-auth--get server :signing-in))))))
          (if headers (json-encode headers) "{}"))
      (scooter-auth-busy "null"))))

(defun scooter-auth-forget (server)
  "Drop SERVER's cached ID token and cookie; the next request logs in again."
  (interactive (list (scooter-current-server t)))
  (scooter-auth--put server :id-token nil :cookie nil :cookie-refused t))

(defun scooter-auth--get (server key)
  "Return SERVER's cached auth KEY."
  (plist-get (gethash (scooter-server--name server) scooter-auth--cache) key))

(defun scooter-auth--put (server &rest kvs)
  "Store KVS in SERVER's auth cache."
  (let ((state (gethash (scooter-server--name server) scooter-auth--cache)))
    (while kvs
      (setq state (plist-put state (pop kvs) (pop kvs))))
    (puthash (scooter-server--name server) state scooter-auth--cache)))

;;;; Cookie

(defun scooter-auth--cookie (server)
  "Return SERVER's oauth2-proxy cookie, asking when there is none."
  (or (scooter-auth--get server :cookie)
      ;; Once refused, a stored cookie is stale until replaced.
      (when-let* (((not (scooter-auth--get server :cookie-refused)))
                  (stored (scooter-auth--secret (scooter-server-host server) "_oauth2_proxy")))
        (scooter-auth--put server :cookie stored)
        stored)
      (scooter-auth--signing-in server (if (plist-get (cdr (scooter-auth--backend server)) :browser)
                                           #'scooter-auth--browser-sign-in
                                         #'scooter-auth-set-cookie))))

(define-error 'scooter-auth-busy "Already signing in" 'user-error)

(defun scooter-auth--signing-in (server sign-in)
  "Call SIGN-IN with SERVER, refusing to nest a second one.
Another shell's bridge asking for headers while the prompt is open runs
inside it (emacsclient is served from the prompt's input loop), so it
signals `scooter-auth-busy' rather than open a second prompt;
`scooter-auth-headers-json' turns that into a \"try again\" answer."
  (when (scooter-auth--get server :signing-in)
    (signal 'scooter-auth-busy
            (list (format "Scooter[%s]: already signing in; finish that first" server))))
  (scooter-auth--put server :signing-in t)
  (unwind-protect (funcall sign-in server)
    (scooter-auth--put server :signing-in nil)))

;;;###autoload
(defun scooter-auth-set-cookie (server &optional cookie)
  "Set SERVER's `_oauth2_proxy' COOKIE, opening the login page to get one."
  (interactive (list (scooter-current-server t)))
  (unless cookie
    (when (y-or-n-p (format "Scooter[%s]: open the login page in a browser? " server))
      (browse-url (scooter-server-url server "/oauth2/sign_in")))
    (setq cookie (string-trim (read-passwd
                               (format "Scooter[%s] _oauth2_proxy cookie value: " server)))))
  (setq cookie (string-remove-prefix "_oauth2_proxy=" cookie))
  (scooter-auth--put server :cookie cookie :cookie-refused nil)
  (scooter-auth--save-secret (scooter-server-host server) "_oauth2_proxy" cookie)
  cookie)

;;;; Browser sign-in through Firefox

(defcustom scooter-auth-firefox-directories
  '("~/.mozilla/firefox" "~/.config/mozilla/firefox"
    "~/Library/Application Support/Firefox"
    "~/snap/firefox/common/.mozilla/firefox"
    "~/.var/app/org.mozilla.firefox/.mozilla/firefox")
  "Where to look for Firefox's profiles.ini; the first one found wins."
  :type '(repeat directory)
  :group 'scooter)

(defcustom scooter-auth-firefox-profile nil
  "The Firefox profile directory to read cookies from.
nil means the one Firefox opens by default, per profiles.ini."
  :type '(choice (const :tag "Default profile" nil) directory)
  :group 'scooter)

(defcustom scooter-auth-browser-check-path "/oauth2/auth"
  "Path on the server that answers 2xx only to a signed-in cookie.
The default is oauth2-proxy's own auth check."
  :type 'string
  :group 'scooter)

(defcustom scooter-auth-browser-poll-interval 1
  "Seconds between looks at the Firefox profile during a sign-in."
  :type 'number
  :group 'scooter)

(defun scooter-auth--ini-sections (file)
  "Return FILE's INI sections as a list of (NAME . ALIST)."
  (with-temp-buffer
    (insert-file-contents file)
    (let (sections)
      (dolist (line (split-string (buffer-string) "\r?\n"))
        (setq line (string-trim line))
        (cond
         ((string-match "\\`\\[\\(.+\\)\\]\\'" line)
          (push (list (match-string 1 line)) sections))
         ((and sections (string-match "\\`\\([^=;#]+?\\)[ \t]*=[ \t]*\\(.*\\)\\'" line))
          (setcdr (car sections) (cons (cons (match-string 1 line) (match-string 2 line))
                                       (cdar sections))))))
      (nreverse sections))))

(defun scooter-auth--firefox-profile ()
  "Return the Firefox profile directory to read cookies from, or nil."
  (or scooter-auth-firefox-profile
      (when-let* ((root (seq-find (lambda (dir) (file-exists-p (expand-file-name "profiles.ini" dir)))
                                  scooter-auth-firefox-directories))
                  (sections (scooter-auth--ini-sections (expand-file-name "profiles.ini" root))))
        (let* ((get (lambda (section key) (cdr (assoc key (cdr section)))))
               (installed (seq-find (lambda (s) (and (string-prefix-p "Install" (car s))
                                                     (funcall get s "Default")))
                                    sections))
               (profiles (seq-filter (lambda (s) (string-prefix-p "Profile" (car s))) sections))
               (profile (or (seq-find (lambda (s) (equal (funcall get s "Default") "1")) profiles)
                            (car profiles))))
          (cond
           (installed (expand-file-name (funcall get installed "Default") root))
           ((funcall get profile "Path")
            (if (equal (funcall get profile "IsRelative") "0")
                (funcall get profile "Path")
              (expand-file-name (funcall get profile "Path") root))))))))

(defun scooter-auth--cookie-hosts (host)
  "Return the moz_cookies hosts whose cookies a request to HOST carries.
Most specific first: HOST's own cookies, then domain cookies for HOST
and each parent domain."
  (let ((labels (split-string host "\\.")) hosts)
    (while labels
      (push (concat "." (string-join labels ".")) hosts)
      (setq labels (cdr labels)))
    (cons host (nreverse hosts))))

(defun scooter-auth--firefox-cookies (profile host)
  "Return the `_oauth2_proxy' cookie values PROFILE would send to HOST.
Best candidates first.  A session oauth2-proxy split into
`_oauth2_proxy_0', `_oauth2_proxy_1', ... comes back joined.  Reads a
copy of cookies.sqlite and its WAL, so Firefox's lock and unflushed
writes don't matter."
  (let ((source (expand-file-name "cookies.sqlite" profile)))
    (when (and (sqlite-available-p) (file-exists-p source))
      (let* ((dir (make-temp-file "scooter-cookies" t))
             (copy (expand-file-name "cookies.sqlite" dir))
             (hosts (scooter-auth--cookie-hosts host))
             rows)
        (unwind-protect
            (progn
              (dolist (suffix '("" "-wal" "-shm"))
                (when (file-exists-p (concat source suffix))
                  (copy-file (concat source suffix) (concat copy suffix) t)))
              (let ((db (sqlite-open copy)))
                (unwind-protect
                    (setq rows (sqlite-select
                                db (format "SELECT host, originAttributes, name, value FROM moz_cookies
 WHERE host IN (%s) AND (name = '_oauth2_proxy' OR name GLOB '_oauth2_proxy_[0-9]*')
 ORDER BY originAttributes != '', lastAccessed DESC"
                                           (mapconcat (lambda (_) "?") hosts ","))
                                hosts))
                  (sqlite-close db))))
          (delete-directory dir t))
        (let (values)
          (dolist (h hosts)
            (dolist (origin (delete-dups (mapcar #'cadr (seq-filter (lambda (r) (equal (car r) h)) rows))))
              (let* ((jar (seq-filter (lambda (r) (and (equal (car r) h) (equal (cadr r) origin))) rows))
                     (whole (seq-find (lambda (r) (equal (nth 2 r) "_oauth2_proxy")) jar))
                     (index (lambda (r) (string-to-number (substring (nth 2 r) 14))))
                     (parts (sort (seq-remove (lambda (r) (eq r whole)) jar)
                                  (lambda (a b) (< (funcall index a) (funcall index b))))))
                (push (if whole (nth 3 whole) (mapconcat (lambda (r) (nth 3 r)) parts "")) values))))
          (delete-dups (delete "" (nreverse values))))))))

(defun scooter-auth--cookie-valid-p (server cookie)
  "Non-nil when SERVER's auth proxy accepts the `_oauth2_proxy' COOKIE."
  (pcase (ignore-errors
           (scooter-http-curl "GET" (scooter-server-url server scooter-auth-browser-check-path)
                              :headers `(("Cookie" . ,(concat "_oauth2_proxy=" cookie)))))
    (`(,status ,ctype ,_) (and (<= 200 status 299) (not (scooter-http-auth-wall-p status ctype))))))

(defun scooter-auth--browser-sign-in (server)
  "Return a signed-in cookie for SERVER from Firefox, signing in there if needed.
Without a Firefox profile (or sqlite support), ask to paste one."
  (let ((profile (scooter-auth--firefox-profile))
        (rejected nil))
    (if (not (and profile (sqlite-available-p) (file-directory-p profile)))
        (progn
          (message "Scooter[%s]: no Firefox profile found; paste the cookie instead" server)
          (scooter-auth-set-cookie server))
      (let* ((look (lambda ()
                     (seq-find (lambda (value)
                                 (or (scooter-auth--cookie-valid-p server value)
                                     (progn (push value rejected) nil)))
                               (seq-difference (ignore-errors (scooter-auth--firefox-cookies
                                                               profile (scooter-server-host server)))
                                               rejected))))
             (found (funcall look)))
        (unless found
          (let ((rd (url-filename (url-generic-parse-url (scooter-server-url server)))))
            (browse-url (scooter-server-url
                         server (concat "/oauth2/start?rd="
                                        (url-hexify-string (if (string-empty-p rd) "/" rd))))))
          (message "Scooter[%s]: sign in in your browser (C-g cancels)..." server)
          (with-timeout (scooter-auth-login-timeout
                         (user-error "Scooter[%s]: sign-in not finished after %d seconds"
                                     server scooter-auth-login-timeout))
            (while (not (setq found (funcall look)))
              (accept-process-output nil scooter-auth-browser-poll-interval))))
        (message "Scooter[%s]: signed in" server)
        (scooter-auth-set-cookie server found)))))

;;;###autoload
(defun scooter-auth-browser-login (server)
  "Sign in to SERVER through Firefox and save the session cookie.
Reuses Firefox's session when it has a live one; otherwise opens the
sign-in page there and waits for it to finish."
  (interactive (list (scooter-current-server t)))
  (scooter-auth--signing-in server #'scooter-auth--browser-sign-in))

;;;; auth-source

(defun scooter-auth--secret (host user)
  "Return the auth-source secret for HOST and USER, or nil."
  (when-let* ((found (car (auth-source-search :host host :user user :max 1)))
              (secret (plist-get found :secret)))
    (if (functionp secret) (funcall secret) secret)))

(defun scooter-auth--save-secret (host user secret)
  "Save SECRET for HOST and USER through auth-source, replacing the old one."
  (ignore-errors
    (auth-source-forget (list :host host :user user))
    (unless (equal (scooter-auth--secret host user) secret)
      ;; `:create' only adds an entry when none matches.
      (scooter-auth--delete-secret host user)
      (let ((found (car (auth-source-search :host host :user user :port "https"
                                            :secret secret :create t :max 1))))
        (when-let* ((save (plist-get found :save-function)))
          (funcall save))))))

(defun scooter-auth--delete-secret (host user)
  "Delete HOST and USER's entries from every auth-source backend."
  (dolist (backend (mapcar #'auth-source-backend-parse auth-sources))
    (let ((source (oref backend source)))
      (ignore-errors
        (pcase (oref backend type)
          ('netrc (scooter-auth--netrc-delete source host user))
          ('secrets (dolist (item (secrets-search-items source :host host :user user))
                      (secrets-delete-item source item)))
          ('plstore (auth-source-search :backend backend :host host :user user
                                        :delete t :max 100))))))
  (auth-source-forget-all-cached))

(defun scooter-auth--netrc-delete (file host user)
  "Delete the one-line netrc entries for HOST and USER from FILE."
  (when (file-exists-p file)
    (with-temp-buffer
      (insert-file-contents file)
      (let ((machine (rx (or bol blank) "machine" (+ blank) (literal host) (or blank eol)))
            (login (rx (or bol blank) (or "login" "user") (+ blank) (literal user) (or blank eol)))
            deleted)
        (goto-char (point-min))
        (while (not (eobp))
          (let ((line (buffer-substring (pos-bol) (pos-eol))))
            (if (and (string-match-p machine line) (string-match-p login line))
                (progn (delete-region (pos-bol) (min (point-max) (1+ (pos-eol))))
                       (setq deleted t))
              (forward-line 1))))
        (when deleted
          (write-region (point-min) (point-max) file nil 'silent)
          (set-file-modes file #o600))))))

;;;; OIDC

(defun scooter-auth--jwt-exp (token)
  "Return the exp claim of JWT TOKEN, or 0."
  (or (ignore-errors
        (let* ((payload (nth 1 (split-string token "\\.")))
               (pad (make-string (% (- 4 (% (length payload) 4)) 4) ?=))
               (json (base64-decode-string (concat payload pad) t)))
          (alist-get 'exp (scooter-http-parse (decode-coding-string json 'utf-8)))))
      0))

(defun scooter-auth--discovery (issuer)
  "Return ISSUER's OpenID discovery document."
  (pcase-let ((`(,status ,_ ,text)
               (scooter-http-curl "GET" (concat (string-trim-right issuer "/")
                                                "/.well-known/openid-configuration"))))
    (unless (= status 200)
      (user-error "OIDC discovery for %s failed: %d" issuer status))
    (scooter-http-parse text)))

(defun scooter-auth--token-request (endpoint form)
  "POST FORM (an alist) to the token ENDPOINT and return the reply."
  (pcase-let ((`(,status ,_ ,text)
               (scooter-http-curl "POST" endpoint
                                  :body (url-build-query-string
                                         (mapcar (lambda (kv) (list (car kv) (cdr kv))) form))
                                  :content-type "application/x-www-form-urlencoded")))
    (let ((reply (scooter-http-parse text)))
      (unless (= status 200)
        (error "Token request failed: %d %s" status (or (alist-get 'error reply) text)))
      reply)))

(defun scooter-auth--id-token (server opts)
  "Return a live ID token for SERVER configured by OPTS."
  (let ((token (scooter-auth--get server :id-token)))
    (if (and token (> (scooter-auth--jwt-exp token) (+ (float-time) 60)))
        token
      (or (ignore-errors (scooter-auth--refresh server opts))
          (scooter-auth--signing-in server #'scooter-auth-login)))))

(defun scooter-auth--store (server opts reply)
  "Cache the tokens in REPLY for SERVER, persisting the refresh token per OPTS."
  (let ((id (alist-get 'id_token reply))
        (refresh (alist-get 'refresh_token reply)))
    (unless id (error "Scooter[%s]: the IdP returned no id_token" server))
    (scooter-auth--put server :id-token id)
    (when refresh
      (scooter-auth--put server :refresh-token refresh)
      (scooter-auth--save-secret (url-host (url-generic-parse-url (plist-get opts :issuer)))
                                 (plist-get opts :client-id) refresh))
    id))

(defun scooter-auth--refresh (server opts)
  "Refresh SERVER's ID token per OPTS, or return nil."
  (when-let* ((refresh (or (scooter-auth--get server :refresh-token)
                           (scooter-auth--secret
                            (url-host (url-generic-parse-url (plist-get opts :issuer)))
                            (plist-get opts :client-id)))))
    (let ((doc (scooter-auth--discovery (plist-get opts :issuer))))
      (scooter-auth--store
       server opts
       (scooter-auth--token-request
        (alist-get 'token_endpoint doc)
        `(("grant_type" . "refresh_token") ("refresh_token" . ,refresh)
          ("client_id" . ,(plist-get opts :client-id))))))))

(defun scooter-auth--random (n)
  "Return N random bytes, base64url-encoded."
  (base64url-encode-string
   (if (and (file-readable-p "/dev/urandom") (executable-find "head"))
       (with-temp-buffer
         (set-buffer-multibyte nil)
         (let ((coding-system-for-read 'binary))
           (call-process "head" "/dev/urandom" t nil "-c" (number-to-string n)))
         (buffer-string))
     ;; No OS entropy file: fall back to the seeded PRNG.
     (random t)
     (apply #'unibyte-string (cl-loop repeat n collect (random 256))))
   t))

(defun scooter-auth-pkce-challenge (verifier)
  "Return the S256 PKCE challenge for VERIFIER."
  (base64url-encode-string (secure-hash 'sha256 verifier nil nil t) t))

;;;###autoload
(defun scooter-auth-login (server)
  "Log in to SERVER's IdP in a browser; return the ID token."
  (interactive (list (scooter-current-server t)))
  (let* ((opts (cdr (scooter-auth--backend server)))
         (doc (scooter-auth--discovery (plist-get opts :issuer)))
         (verifier (scooter-auth--random 48))
         (state (scooter-auth--random 16))
         (result nil)
         (proc (make-network-process
                :name "scooter-oidc-callback" :server t :host "127.0.0.1" :service t
                :family 'ipv4 :noquery t
                :filter (lambda (conn text)
                          (when (string-match "\\`GET \\([^ ]+\\)" text)
                            (let* ((query (url-parse-query-string
                                           (or (cadr (split-string (match-string 1 text) "?")) "")))
                                   (get (lambda (k) (cadr (assoc k query)))))
                              (setq result (if (equal (funcall get "state") state)
                                               (or (funcall get "code")
                                                   (list 'error (funcall get "error")))
                                             (list 'error "state mismatch")))
                              (process-send-string
                               conn (concat "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\n"
                                            "Scooter: you can close this tab.\n"))
                              (delete-process conn))))))
         (redirect (format "http://127.0.0.1:%d/callback" (process-contact proc :service))))
    (unwind-protect
        (progn
          (browse-url
           (concat (alist-get 'authorization_endpoint doc) "?"
                   (url-build-query-string
                    `(("response_type" "code")
                      ("client_id" ,(plist-get opts :client-id))
                      ("redirect_uri" ,redirect)
                      ("scope" ,(string-join (or (plist-get opts :scopes)
                                                 '("openid" "email" "profile" "offline_access"))
                                             " "))
                      ("state" ,state)
                      ("code_challenge" ,(scooter-auth-pkce-challenge verifier))
                      ("code_challenge_method" "S256")))))
          (message "Scooter[%s]: finish the login in your browser..." server)
          (with-timeout (scooter-auth-login-timeout
                         (user-error "Scooter[%s]: login timed out" server))
            (while (not result) (accept-process-output nil 0.2)))
          (when (consp result)
            (user-error "Scooter[%s]: login failed: %s" server (cadr result)))
          (prog1 (scooter-auth--store
                  server opts
                  (scooter-auth--token-request
                   (alist-get 'token_endpoint doc)
                   `(("grant_type" . "authorization_code") ("code" . ,result)
                     ("redirect_uri" . ,redirect) ("code_verifier" . ,verifier)
                     ("client_id" . ,(plist-get opts :client-id)))))
            (message "Scooter[%s]: logged in" server)))
      (delete-process proc))))

(provide 'scooter-auth)
;;; scooter-auth.el ends here
