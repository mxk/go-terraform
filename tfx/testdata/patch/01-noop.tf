resource "test_resource" "r1" {
  required = "a"

  required_map {
    k1 = 1
  }
}
