;;; fake-server.el --- A tiny Scooter stand-in for tests  -*- lexical-binding: t; -*-

;; Run with emacs --batch -l fake-server.el; prints its port, then serves:
;; /whoami: JSON when the cookie is "good", else a 302 to the login page.
;; /forbidden: Scooter's own JSON 403.
;; /oauth2/auth: oauth2-proxy's check, 202 for the "good" cookie, else 401.
;; /conversations: two conversations; /conversations/events: an empty stream.

(defun fake--reply (conn status ctype body)
  (process-send-string
   conn (format "HTTP/1.1 %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s"
                status ctype (string-bytes body) body))
  (process-send-eof conn))

(let ((proc (make-network-process
             :name "fake" :server t :host "127.0.0.1" :service t :family 'ipv4
             :filter (lambda (conn text)
                       (when (string-match "\r\n\r\n" text)
                         (cond
                          ((string-match-p "\\`GET /conversations " text)
                           (fake--reply conn "200 OK" "application/json"
                                        "[{\"id\":\"c1\",\"title\":\"Fix login\",\"model\":\"m1\",\"starred\":true,\"runStartedAt\":1,\"lastActivityAt\":1},{\"id\":\"0123456789\",\"status\":\"suspended\"}]"))
                          ((string-match-p "\\`GET /conversations/events" text)
                           (fake--reply conn "200 OK" "text/event-stream" ""))
                          ((string-match-p "\\`[A-Z]+ /forbidden" text)
                           (fake--reply conn "403 Forbidden" "application/json" "{\"error\":\"not your conversation\"}"))
                          ((string-match-p "\\`GET /oauth2/auth " text)
                           (fake--reply conn (if (string-match-p "^Cookie: _oauth2_proxy=good\r" text)
                                                 "202 Accepted" "401 Unauthorized")
                                        "text/plain" ""))
                          ((string-match-p "^Cookie: _oauth2_proxy=good" text)
                           (fake--reply conn "200 OK" "application/json" "{\"id\":\"me\"}"))
                          (t (process-send-string
                              conn "HTTP/1.1 302 Found\r\nLocation: /oauth2/start\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
                             (process-send-eof conn))))))))
  (princ (format "%d\n" (process-contact proc :service)))
  (while t (accept-process-output nil 1)))
