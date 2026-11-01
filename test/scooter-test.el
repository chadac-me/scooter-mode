;;; scooter-test.el --- Tests for scooter-mode  -*- lexical-binding: t; -*-

(require 'ert)
(require 'scooter)

;; Keep tests away from the user's auth-source files.
(setq auth-sources nil)

(ert-deftest scooter-test-slug ()
  (should (equal (scooter-slug "Fix the login redirect!" "abc") "fix-the-login-redirect"))
  (should (equal (scooter-slug "  --Ünïcode & spaces--  " "abc") "n-code-spaces"))
  (should (equal (scooter-slug nil "0123456789abcdef") "01234567"))
  (should (equal (scooter-slug "" "abc") "abc"))
  (should (<= (length (scooter-slug (make-string 60 ?a) "x")) 40)))

(ert-deftest scooter-test-groups ()
  (should (eq (scooter-list-group '((status . "running") (awaitingInputAt . 5) (runStartedAt . 1))) 'awaiting))
  (should (eq (scooter-list-group '((status . "running") (runStartedAt . 1))) 'running))
  (should (eq (scooter-list-group '((status . "running"))) 'idle))
  (should (eq (scooter-list-group '((status . "suspended"))) 'suspended))
  (should (eq (scooter-list-group '((status . "failed"))) 'failed)))

(ert-deftest scooter-test-auth-wall ()
  (should (scooter-http-auth-wall-p 401 "application/json"))
  (should (scooter-http-auth-wall-p 302 ""))
  (should (scooter-http-auth-wall-p 403 "text/html"))
  (should (scooter-http-auth-wall-p 200 "text/html; charset=utf-8"))
  (should-not (scooter-http-auth-wall-p 403 "application/json"))
  (should-not (scooter-http-auth-wall-p 404 "application/json"))
  (should-not (scooter-http-auth-wall-p 200 "application/json")))

(ert-deftest scooter-test-jwt-exp ()
  (let ((token (concat "e30." (base64url-encode-string "{\"exp\":1700000000}" t) ".sig")))
    (should (= (scooter-auth--jwt-exp token) 1700000000)))
  (should (= (scooter-auth--jwt-exp "garbage") 0)))

(ert-deftest scooter-test-pkce ()
  ;; Expected value computed independently with Perl's Digest::SHA.
  (should (equal (scooter-auth-pkce-challenge "dBjftJeZ4CVP-mJ92K1hgPLBEsl4XBO6pqR1Vie9rF8")
                 "niQw9d7JkLeYdv9DhxacziU9zeJo1l1b1rGVtP0T2i4"))
  (let ((a (scooter-auth--random 32)) (b (scooter-auth--random 32)))
    (should (= (length a) 43))
    (should-not (equal a b))))

(ert-deftest scooter-test-cookie-replaces-saved-one ()
  (let* ((file (make-temp-file "scooter-authinfo"))
         (auth-sources (list file))
         (auth-source-save-behavior t)
         (scooter-servers '((home :url "https://s.example" :auth cookie)))
         (scooter-auth--cache (make-hash-table :test #'eq)))
    (unwind-protect
        (progn
          (with-temp-file file
            (insert "machine other.example login me password keep\n"
                    "machine s.example login _oauth2_proxy password stale\n"))
          (scooter-auth-set-cookie 'home "fresh")
          ;; A restart: only auth-source remembers the cookie.
          (clrhash scooter-auth--cache)
          (auth-source-forget-all-cached)
          (should (equal (scooter-auth--cookie 'home) "fresh"))
          (with-temp-buffer
            (insert-file-contents file)
            (should (= (count-matches "_oauth2_proxy" (point-min) (point-max)) 1))
            (should (search-forward "password keep" nil t))))
      (delete-file file))))

(ert-deftest scooter-test-headers-json ()
  (let ((scooter-servers '((home :url "https://s.example" :auth cookie)
                           (dev :url "http://localhost:8080")))
        (scooter-auth--cache (make-hash-table :test #'eq)))
    (scooter-auth-set-cookie 'home "_oauth2_proxy=abc")
    (should (equal (json-parse-string (scooter-auth-headers-json "home") :object-type 'alist)
                   '((Cookie . "_oauth2_proxy=abc"))))
    (should (equal (scooter-auth-headers-json "dev") "{}"))))

(ert-deftest scooter-test-headers-command-keeps-placeholder ()
  (let ((server-socket-dir "/tmp/emacs-sock") (server-name "server"))
    (should (string-match-p "{retry})'\\'" (scooter-agent-shell-headers-command 'home)))))

(ert-deftest scooter-test-acp-arguments ()
  (let ((scooter-servers '((home :url "https://s.example/" :model "m1")))
        (scooter-acp-command '("scooter" "acp")))
    (cl-letf (((symbol-function 'scooter-agent-shell--headers-args) (lambda (_) '("--headers-file" "f"))))
      (should (equal (scooter-acp-arguments 'home :project t)
                     '("acp" "--server" "https://s.example" "--model" "m1" "--project" "--headers-file" "f"))))))

(ert-deftest scooter-test-acp-arguments-self ()
  (let ((scooter-servers '((sandbox :self t)))
        (scooter-acp-command '("scooter" "acp")))
    (should (equal (scooter-acp-arguments 'sandbox) '("acp" "--self")))))

(defconst scooter-test--dir (file-name-directory (or load-file-name buffer-file-name))
  "This file's directory, captured at load time.")

(defmacro scooter-test-with-fake-server (url-var &rest body)
  "Run BODY with URL-VAR bound to a fake Scooter's base URL."
  (declare (indent 1))
  `(let* ((proc (start-process "fake-scooter" (generate-new-buffer " *fake*") "emacs" "-Q" "--batch" "-l"
                               (expand-file-name "fake-server.el" scooter-test--dir)))
          (port nil))
     (unwind-protect
         (progn
           (with-current-buffer (process-buffer proc)
             (with-timeout (10 (error "Fake server didn't start"))
               (while (not (string-match "^\\([0-9]+\\)$" (buffer-string)))
                 (accept-process-output proc 0.1)))
             (setq port (match-string 1 (buffer-string))))
           (let ((,url-var (format "http://127.0.0.1:%s" port)))
             ,@body))
       (delete-process proc))))

(ert-deftest scooter-test-request-retries-after-auth-wall ()
  (scooter-test-with-fake-server url
    (let ((scooter-servers `((home :url ,url :auth cookie)))
          (scooter-auth--cache (make-hash-table :test #'eq))
          (asked 0))
      (scooter-auth-set-cookie 'home "stale")
      (cl-letf (((symbol-function 'scooter-auth--secret) #'ignore)
                ((symbol-function 'read-passwd) (lambda (&rest _) (cl-incf asked) "good"))
                ((symbol-function 'y-or-n-p) #'ignore))
        (should (equal (scooter-request 'home "GET" "/whoami") '((id . "me"))))
        (should (= asked 1))
        ;; The fresh cookie is reused.
        (should (equal (scooter-request 'home "GET" "/whoami") '((id . "me"))))
        (should (= asked 1))))))

(ert-deftest scooter-test-request-gives-up-after-one-retry ()
  (scooter-test-with-fake-server url
    (let ((scooter-servers `((home :url ,url :auth cookie)))
          (scooter-auth--cache (make-hash-table :test #'eq)))
      (scooter-auth-set-cookie 'home "stale")
      (cl-letf (((symbol-function 'scooter-auth--secret) #'ignore)
                ((symbol-function 'read-passwd) (lambda (&rest _) "still-bad"))
                ((symbol-function 'y-or-n-p) #'ignore))
        (should-error (scooter-request 'home "GET" "/whoami") :type 'scooter-auth-wall)))))

(ert-deftest scooter-test-json-403-is-an-error-not-a-wall ()
  (scooter-test-with-fake-server url
    (let ((scooter-servers `((home :url ,url))))
      (let ((err (should-error (scooter-request 'home "POST" "/forbidden") :type 'scooter-http-error)))
        (should (string-match-p "not your conversation" (format "%s" err)))))))




(ert-deftest scooter-test-agent-shell-config-runs-the-bridge ()
  (let ((scooter-servers '((home :url "https://s.example")))
        (scooter-acp-command '("scooter" "acp")))
    (cl-letf (((symbol-function 'scooter-agent-shell--headers-args) (lambda (_) '("--headers-file" "f")))
              ((symbol-function 'scooter-install-program) #'identity))
      (let* ((config (scooter-agent-shell-config 'home :title "fix-login"))
             (client (with-temp-buffer (funcall (map-elt config :client-maker) (current-buffer)))))
        (should (equal (map-elt config :buffer-name) "Scooter fix-login@home"))
        (should (member "--server" (map-elt client :command-params)))
        (should (equal (cons (map-elt client :command) (map-elt client :command-params))
                       '("scooter" "acp" "--server" "https://s.example" "--headers-file" "f")))))))

(ert-deftest scooter-test-install-verifies-and-writes ()
  (let* ((bin (unibyte-string 0 127 128 255 10 13))
         (asset (concat "scooter-" (scooter-install--platform)))
         (user-emacs-directory (make-temp-file "scooter-ued" t))
         (sums (format "%s  %s\n" (secure-hash 'sha256 bin) asset)))
    (cl-letf (((symbol-function 'scooter-install--fetch)
               (lambda (url) (if (string-suffix-p "SHA256SUMS" url) sums bin))))
      (let ((path (scooter-install "9.9.9")))
        (should (file-executable-p path))
        (should (equal (with-temp-buffer
                         (set-buffer-multibyte nil)
                         (insert-file-contents-literally path)
                         (buffer-string))
                       bin))))))

(ert-deftest scooter-test-install-rejects-bad-checksum ()
  (let ((user-emacs-directory (make-temp-file "scooter-ued" t))
        (asset (concat "scooter-" (scooter-install--platform))))
    (cl-letf (((symbol-function 'scooter-install--fetch)
               (lambda (url) (if (string-suffix-p "SHA256SUMS" url)
                                 (format "%s  %s\n" (make-string 64 ?0) asset)
                               "tampered"))))
      (should-error (scooter-install "9.9.9"))
      (should-not (file-exists-p (scooter-install-path "9.9.9"))))))

(ert-deftest scooter-test-install-program-prefers-path ()
  (let ((scooter-install-binary nil))
    (should (equal (scooter-install-program "sh") (executable-find "sh")))
    (should-error (scooter-install-program "no-such-scooter-binary") :type 'user-error)))

(defun scooter-test--lisp-dir ()
  "The package's lisp/ directory."
  (file-name-directory (locate-library "scooter")))

(ert-deftest scooter-test-list-from-autoloads-in-fresh-emacs ()
  ;; One process with everything loaded hides missing requires; start cold.
  (scooter-test-with-fake-server url
    (let* ((dir (make-temp-file "scooter-autoloads" t))
           (defs (expand-file-name "scooter-autoloads.el" dir))
           (form `(progn
                    (setq load-path ',load-path)
                    (load ,defs nil t)
                    (setq scooter-servers '((home :url ,url)))
                    (scooter-list 'home)
                    (princ (with-current-buffer "*scooter: home*" (buffer-string))))))
      (loaddefs-generate (scooter-test--lisp-dir) defs)
      (with-temp-buffer
        (let ((rc (call-process "emacs" nil t nil "-Q" "--batch" "--eval" (prin1-to-string form))))
          (should (= rc 0))
          (should (string-match-p "^Running (1)\n★ fix-login +[0-9]+d  m1$" (buffer-string)))
          (should (string-match-p "^Suspended (1)$" (buffer-string)))
          (should-not (string-match-p "void" (buffer-string))))))))

(defmacro scooter-test-with-list (convs &rest body)
  "Run BODY in a list buffer showing CONVS on server home."
  (declare (indent 1))
  `(let ((scooter-servers '((home :url "https://s.example"))))
     (cl-letf (((symbol-function 'scooter-list--fetch) (lambda (_) ,convs))
               ((symbol-function 'scooter-list--start-streams) #'ignore))
       (save-window-excursion
         (scooter-list 'home)
         (unwind-protect (progn ,@body)
           (kill-buffer "*scooter: home*"))))))

(ert-deftest scooter-test-list-rows-align ()
  (let ((now (* 1000 (float-time))))
    (scooter-test-with-list
        (list `((id . "a") (title . "Short") (starred . t) (runStartedAt . 1) (lastActivityAt . ,(- now 540000)))
              `((id . "b") (title . "A much longer conversation title") (model . "m2") (runStartedAt . 1)
                (lastActivityAt . ,(- now 7200000))))
      (should (string-match-p "^★ short +9m$" (buffer-string)))
      (let ((cols (mapcar (lambda (re) (goto-char (point-min)) (re-search-forward re) (current-column))
                          '("9m" "2h"))))
        (should (apply #'= cols))))))

(ert-deftest scooter-test-list-faces ()
  (scooter-test-with-list
      (list '((id . "a") (title . "Wait") (awaitingInputAt . 1) (model . "m1"))
            '((id . "b") (title . "Run") (runStartedAt . 1))
            '((id . "c") (title . "Boom") (status . "failed") (starred . t)))
    (cl-flet ((face-at (text) (goto-char (point-min)) (search-forward text)
                (get-text-property (match-beginning 0) 'face)))
      (should (eq (face-at "Awaiting you") 'scooter-list-awaiting))
      (should (eq (face-at "Running") 'scooter-list-running))
      (should (eq (face-at "Failed") 'scooter-list-failed))
      (should (eq (face-at "m1") 'scooter-list-model)))))

(ert-deftest scooter-test-list-shows-fetch-errors ()
  (scooter-test-with-list "Scooter[home]: login required"
    (should (string-match-p "^  Scooter\\[home\\]: login required$" (buffer-string)))))

(ert-deftest scooter-test-list-help-key ()
  (with-temp-buffer
    (scooter-list-mode)
    (should (eq (key-binding "?") #'scooter-list-dispatch))))

(defun scooter-test--fresh-emacs (&rest forms)
  "Run FORMS in a fresh Emacs with this `load-path'; return (EXIT . OUTPUT)."
  (with-temp-buffer
    (let ((rc (call-process (expand-file-name invocation-name invocation-directory)
                            nil t nil "-Q" "--batch" "--eval"
                            (prin1-to-string `(progn (setq load-path ',load-path) ,@forms)))))
      (cons rc (buffer-string)))))

(defmacro scooter-test--skip-unless-installed (feature)
  "Skip unless FEATURE is installed; Cask installs it from MELPA."
  `(skip-unless (locate-library ,(symbol-name feature))))

(ert-deftest scooter-test-projectile-binds-s ()
  ;; Real MELPA Projectile, loaded before and after scooter-project.
  (scooter-test--skip-unless-installed projectile)
  (dolist (order '((projectile scooter-project) (scooter-project projectile)))
    (let ((res (scooter-test--fresh-emacs
                `(mapc #'require ',order)
                '(princ (keymap-lookup projectile-command-map "S")))))
      (should (equal res '(0 . "scooter-new-from-project"))))))

(ert-deftest scooter-test-list-under-real-evil ()
  ;; Real MELPA evil: the list opens in Emacs state so its keys work.
  (scooter-test--skip-unless-installed evil)
  (scooter-test-with-fake-server url
    (let ((res (scooter-test--fresh-emacs
                '(require 'evil) '(evil-mode 1) '(require 'scooter-list)
                `(setq scooter-servers '((home :url ,url)))
                '(scooter-list 'home)
                '(princ (list evil-state (key-binding "?") (key-binding "g"))))))
      (should (equal res '(0 . "(emacs scooter-list-dispatch scooter-list-refresh)"))))))

;;; Tramp

(require 'scooter-tramp)

(ert-deftest scooter-test-tramp-file-name-prefers-a-unique-slug ()
  (cl-letf (((symbol-function 'scooter--title-of) (lambda (_s id) (if (equal id "old") "dup" "fix")))
            ((symbol-function 'scooter-tramp-resolve) (lambda (_s slug) (if (equal slug "dup") "new" "c1"))))
    (should (equal (scooter-tramp-file-name 'home "c1") "/scooter:fix@home:/workspace/"))
    (should (equal (scooter-tramp-file-name 'home "old" "/tmp/") "/scooter:old@home:/tmp/"))))

(ert-deftest scooter-test-tramp-resolves-slugs-and-id-prefixes ()
  (scooter-test-with-fake-server url
    (let ((scooter-servers `((home :url ,url))))
      (should (equal (scooter-tramp-resolve 'home "fix-login") "c1"))
      (should (equal (scooter-tramp-resolve 'home "0123") "0123456789"))
      (should-error (scooter-tramp-resolve 'home "nope") :type 'user-error)
      (should (equal (scooter-tramp-completions "") '(("fix-login" "home") ("01234567" "home")))))))

(ert-deftest scooter-test-tramp-writes-a-private-spec ()
  (scooter-test-with-fake-server url
    (let* ((scooter-servers `((home :url ,url)))
           (scooter-tramp-directory (file-name-as-directory (make-temp-file "scooter-tramp" t)))
           (file (scooter-tramp--file "fix-login.home.json")))
      (unwind-protect
          (progn
            (scooter-tramp--write-spec "home" "fix-login")
            (should (= (file-modes file) #o600))
            (should (equal (json-read-file file)
                           `((url . ,url) (conversation . "c1") (headers)))))
        (delete-directory scooter-tramp-directory t)))))

(ert-deftest scooter-test-tramp-login-args-survive-the-shell ()
  ;; Tramp joins the args with spaces and hands them to a shell.
  (cl-letf (((symbol-function 'scooter-install-program) #'identity))
    (let* ((scooter-tramp-directory "/tmp/a dir/")
           (scooter-acp-command '("/opt/my scooter" "acp"))
           (words (split-string-shell-command
                   (string-join (flatten-tree (scooter-tramp-login-args)) " ")))
           (proxy (seq-find (lambda (w) (string-prefix-p "ProxyCommand=" w)) words)))
      ;; ssh runs the ProxyCommand through a second shell.
      (should (equal (split-string-shell-command (string-remove-prefix "ProxyCommand=" proxy))
                     '("/opt/my scooter" "tunnel" "--spec-file" "/tmp/a dir/%u.%h.json")))
      (should (member "/tmp/a dir/id_ed25519" words))
      (should (equal (last words) '("%h"))))))

(ert-deftest scooter-test-shell-find-file-starts-in-the-sandbox ()
  (let ((scooter-servers '((home :url "https://s.example") (me :url "https://m.example" :self t)))
        (calls 0))
    (cl-letf (((symbol-function 'scooter-tramp-file-name)
               (lambda (server id) (cl-incf calls) (format "/scooter:%s@%s:/workspace/" id server))))
      (with-temp-buffer
        (setq-local agent-shell--state '((:agent-config (:identifier . scooter-home))
                                         (:session (:id . "abc"))))
        (scooter-agent-shell--enable-files)
        (should scooter-agent-shell-files-mode)
        (should (eq (key-binding [remap find-file]) #'scooter-agent-shell-find-file))
        (should (equal (scooter-agent-shell-directory) "/scooter:abc@home:/workspace/"))
        (scooter-agent-shell-directory)
        (should (= calls 1))
        (cl-letf (((symbol-function 'find-file) #'identity))
          (should (equal (call-interactively #'scooter-find-file) "/scooter:abc@home:/workspace/"))))
      (with-temp-buffer
        (setq-local agent-shell--state '((:agent-config (:identifier . scooter-home))))
        (should-not (scooter-agent-shell-directory)))
      (with-temp-buffer
        (setq-local agent-shell--state '((:agent-config (:identifier . scooter-me))
                                         (:session (:id . "abc"))))
        (should-not (scooter-agent-shell-directory)))
      (with-temp-buffer
        (setq-local agent-shell--state '((:agent-config (:identifier . claude-code))))
        (scooter-agent-shell--enable-files)
        (should-not scooter-agent-shell-files-mode)))))

(ert-deftest scooter-test-shell-renamed-after-title ()
  (let ((scooter-servers '((home :url "https://s.example")))
        (title nil))
    (cl-letf (((symbol-function 'scooter-request)
               (lambda (_server _method path)
                 (should (equal path "/conversations/abcdef123456"))
                 `((id . "abcdef123456") (title . ,title))))
              ((symbol-function 'agent-shell--project-name) (lambda () "proj"))
              ((symbol-function 'agent-shell--format-buffer-name)
               (lambda (name project) (format "%s Agent @ %s" name project))))
      (with-temp-buffer
        (rename-buffer "Scooter abcdef12@home Agent @ proj" t)
        (setq-local agent-shell--state
                    '((:agent-config (:identifier . scooter-home) (:buffer-name . "Scooter abcdef12@home"))
                      (:session (:id . "abcdef123456"))))
        (scooter-agent-shell-retitle)
        (should (equal (buffer-name) "Scooter abcdef12@home Agent @ proj"))
        (setq title "Fix the Login redirect")
        (scooter-agent-shell-retitle)
        (should (equal (buffer-name) "Scooter fix-the-login-redirect@home Agent @ proj"))
        (should (equal (map-nested-elt agent-shell--state '(:agent-config :buffer-name))
                       "Scooter fix-the-login-redirect@home"))
        (rename-buffer "mine" t)
        (setq title "Something else")
        (scooter-agent-shell-retitle)
        (should (equal (buffer-name) "mine"))))))

(provide 'scooter-test)
;;; scooter-test.el ends here
