resource "test_resource" "r1" {
  required           = "a"
  optional_force_new = "y"

  required_map {
    k1 = 1
  }
}

resource "test_resource" "r2" {
  required           = "b"
  optional_force_new = "${test_resource.r1.optional_force_new}"

  required_map {
    k1 = 2
  }
}

resource "test_resource" "r3" {
  required           = "c"
  optional_force_new = "${test_resource.r2.optional_force_new}"

  required_map {
    k1 = 3
  }
}
