resource "test_resource" "r1" {
  required           = "b"
  optional_force_new = "x"

  required_map {
    k1 = 2
    k2 = 3
  }
}
