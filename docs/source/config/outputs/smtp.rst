
SmtpOutput
==========

.. versionadded:: 0.5

Outputs a Heka message in an email.  The message subject is the plugin name
and the message content is controlled by the payload_only setting.  The
primary purpose is for email alert notifications e.g., PagerDuty.

Config:

- send_from (string)
    The email address of the sender. (default: "heka@localhost.localdomain")
- send_to (array of strings)
    An array of email addresses where the output will be sent to.
- subject (string)
    Custom subject line of email. (default: "Heka [SmtpOutput]")
- host (string)
    SMTP host to send the email to (default: "127.0.0.1:25")
- auth (string)
    SMTP authentication type: "none", "Plain", "CRAMMD5" (default: "none")
- user (string, optional)
    SMTP user name
- password (string, optional)
    SMTP user password

- encoder (string, required):
    .. versionadded:: 0.6

    Encoder plugin used to format the output.

- payload_only (bool)
    .. deprecated:: 0.6
        Use encoder instead.

    If set to true, then only the message payload string will be emailed,
    otherwise the entire `Message` struct will be emailed in JSON format.
    (default: true)


Example:

.. code-block:: ini

    [FxaAlert]
    type = "SmtpOutput"
    message_matcher = "((Type == 'heka.sandbox-output' && Fields[payload_type] == 'alert') || Type == 'heka.sandbox-terminated') && Logger =~ /^Fxa/"
    send_from = "heka@example.com"
    send_to = ["alert@example.com"]
    auth = "Plain"
    user = "test"
    password = "testpw"
    host = "localhost:25"
    encoder = "AlertEncoder"

