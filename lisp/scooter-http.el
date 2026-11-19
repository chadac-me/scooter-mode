;;; scooter-http.el --- curl-backed requests to Scooter  -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Chad Crawford
;; SPDX-License-Identifier: MIT

;;; Commentary:

;; Requests run curl, which handles streaming, cookies and proxies better than
;; url.el.  Headers go through a 0600 file (`-H @file'), never argv.

;;; Code:

(require 'cl-lib)
(require 'json)
(require 'scooter-server)

(autoload 'scooter-auth-headers "scooter-auth")

(defcustom scooter-curl-program "curl"
  "The curl executable."
  :type 'string
  :group 'scooter)

(defcustom scooter-http-timeout 30
  "Seconds before a one-shot request gives up."
  :type 'integer
  :group 'scooter)

(define-error 'scooter-auth-wall "Scooter login required")
(define-error 'scooter-http-error "Scooter request failed")

(defun scooter-http--headers-file (headers)
  "Write HEADERS, an alist, to a fresh 0600 file and return its name."
  (let ((file (with-file-modes #o600 (make-temp-file "scooter-h"))))
    (with-temp-file file
      (pcase-dolist (`(,k . ,v) headers)
        (insert k ": " v "\n")))
    file))

(defun scooter-http-auth-wall-p (status content-type)
  "Non-nil when STATUS and CONTENT-TYPE come from the auth proxy."
  (let ((json (string-prefix-p "application/json" (or content-type ""))))
    (or (= status 401)
        (and (>= status 300) (< status 400))
        ;; Scooter's own 403s are JSON; the proxy's are not.
        (and (= status 403) (not json))
        (string-prefix-p "text/html" (or content-type "")))))

(cl-defun scooter-http-curl (method url &key headers body content-type)
  "Run one METHOD request to URL and return (STATUS CONTENT-TYPE BODY).
HEADERS is an alist.  BODY is a string sent with CONTENT-TYPE."
  (let ((hfile (scooter-http--headers-file
                (append headers (and body `(("Content-Type" . ,(or content-type "application/json")))))))
        (marker "\n__scooter__"))
    (unwind-protect
        (with-temp-buffer
          (set-buffer-multibyte nil)
          (let* ((coding-system-for-read 'binary)
                 (coding-system-for-write 'utf-8)
                 (args (append (list "-sS" "-X" method "-H" (concat "@" hfile)
                                     "--max-time" (number-to-string scooter-http-timeout)
                                     "-w" (concat marker "%{http_code} %{content_type}"))
                               (and body '("--data-binary" "@-"))
                               (list url)))
                 (exit (if body
                           (with-temp-buffer
                             (insert body)
                             (let ((out (generate-new-buffer " *scooter-curl*")))
                               (with-current-buffer out (set-buffer-multibyte nil))
                               (unwind-protect
                                   (prog1 (apply #'call-process-region (point-min) (point-max)
                                                 scooter-curl-program nil out nil args)
                                     (setq body (with-current-buffer out (buffer-string))))
                                 (kill-buffer out))))
                         (prog1 (apply #'call-process scooter-curl-program nil t nil args)
                           (setq body (buffer-string))))))
            (unless (eql exit 0)
              (signal 'scooter-http-error (list url (format "curl exited %s: %s" exit (string-trim body)))))
            (let* ((at (or (string-search marker body) (length body)))
                   (meta (split-string (substring body (min (length body) (+ at (length marker))))))
                   (text (decode-coding-string (substring body 0 at) 'utf-8)))
              (list (string-to-number (or (car meta) "0")) (or (cadr meta) "") text))))
      (delete-file hfile))))

(defun scooter-http-parse (text)
  "Parse JSON TEXT into alists and lists, or nil when empty."
  (unless (string-empty-p (string-trim text))
    (json-parse-string text :object-type 'alist :array-type 'list
                       :null-object nil :false-object :false)))

(defun scooter-request (server method path &optional body)
  "Send METHOD PATH to SERVER with BODY (encoded as JSON); return the reply.
Reloads auth once on an auth wall, then signals `scooter-auth-wall'."
  (let ((url (scooter-server-api-url server path))
        (payload (and body (json-encode body)))
        (retry nil)
        result)
    (while (not result)
      (pcase-let ((`(,status ,ctype ,text)
                   (scooter-http-curl method url
                                      :headers (scooter-auth-headers server retry)
                                      :body payload)))
        (cond
         ((scooter-http-auth-wall-p status ctype)
          (if retry
              (signal 'scooter-auth-wall (list (format "Scooter[%s]: login required" server)))
            (setq retry t)))
         ((/= (/ status 100) 2)
          (signal 'scooter-http-error
                  (list (format "%s %s: %d %s" method path status
                                (truncate-string-to-width (string-trim text) 200)))))
         (t (setq result (list (scooter-http-parse text)))))))
    (car result)))

(defun scooter-http-stream (server path on-frame)
  "Follow SERVER's SSE stream at PATH, calling ON-FRAME with each parsed frame.
Returns the curl process.  The caller restarts it from the sentinel."
  (let* ((hfile (scooter-http--headers-file (scooter-auth-headers server)))
         (pending "")
         (proc (make-process
                :name (format "scooter-sse-%s" server)
                :buffer nil
                :connection-type 'pipe
                :coding 'utf-8
                :noquery t
                :command (list scooter-curl-program "-sS" "-N" "--fail"
                               "-H" (concat "@" hfile)
                               (scooter-server-api-url server path))
                :filter
                (lambda (_proc chunk)
                  (setq pending (concat pending chunk))
                  (let (end)
                    (while (setq end (string-search "\n\n" pending))
                      (let ((event (substring pending 0 end)))
                        (setq pending (substring pending (+ end 2)))
                        (dolist (line (split-string event "\n"))
                          (when (string-prefix-p "data:" line)
                            (let ((frame (ignore-errors
                                           (scooter-http-parse (string-trim-left (substring line 5))))))
                              (when frame (funcall on-frame frame))))))))))))
    ;; curl has read the headers once it starts; the file can go.
    (run-at-time 5 nil (lambda () (ignore-errors (delete-file hfile))))
    proc))

(provide 'scooter-http)
;;; scooter-http.el ends here
