```hcl
resource "aws_iam_user_policy_attachment" "attachment" {
  user       = "${aws_iam_user.user1.name}"
  policy_arn = "${aws_iam_policy.policy1.arn}"
}

resource "aws_iam_user_group_membership" "membership" {
  user   = "${aws_iam_user.user1.name}"
  groups = [
    "${aws_iam_group.group1.name}",
    "${aws_iam_group.group2.name}",
  ]
}
```
